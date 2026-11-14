package manager_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// A failed visibility update is repaired by a later Sync, and left alone
// once seen right.
func TestVisibilityUpdateThatFailedIsRepairedBySync(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	up.mu.Lock()
	up.patchFails = 1
	up.mu.Unlock()
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("pool-0")
	pod.registerFor(wsA)
	if p := up.patchList(); len(p) != 1 || !strings.HasPrefix(p[0].Body, "FAILED") {
		t.Fatalf("the first update must have failed: %+v", p)
	}

	h.pool.Sync(context.Background()) // sees private, shares again
	if p := up.patchList(); len(p) != 2 || p[1].Body != `{"visibility":"public"}` {
		t.Fatalf("Sync must repair the visibility: %+v", p)
	}
	h.pool.Sync(context.Background()) // sees public, remembers
	gets := up.pathCount("GET /api/runtimes")
	h.pool.Sync(context.Background())
	h.pool.Sync(context.Background())
	if n := up.pathCount("GET /api/runtimes"); n != gets {
		t.Fatalf("a workspace seen right is not checked again: %d -> %d", gets, n)
	}
	if n := len(up.patchList()); n != 2 {
		t.Fatalf("no more updates once public: %d", n)
	}
}

func TestPrivateVisibilityMakesNoVisibilityCalls(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *manager.Config) { c.RuntimeVisibility = "private" })
	h.pool.Sync(context.Background())
	// The first Sync lists the runtimes once to recover orphaned tasks.
	listed := up.pathCount("GET /api/runtimes")
	pod := h.newPod("pool-0")
	pod.registerFor(wsA)
	h.pool.Sync(context.Background())
	if n := up.pathCount("GET /api/runtimes"); n != listed {
		t.Fatalf("private runtimes are never checked: %d calls after the first Sync", n-listed)
	}
	if p := up.patchList(); len(p) != 0 {
		t.Fatalf("private runtimes are never updated: %+v", p)
	}
}

// The registration request may end (the pod gave up waiting) while the
// manager is still sharing the runtime and recovering orphans; both go on.
func TestSharingAndRecoverySurviveTheRegistrationRequestEnding(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	var delivered atomic.Bool
	up.setHook(func(r *http.Request) (int, string) {
		if r.Method == http.MethodPatch {
			time.Sleep(400 * time.Millisecond)
			if r.Context().Err() == nil {
				delivered.Store(true)
			}
		}
		return 0, ""
	})
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("pool-0")
	short := &http.Client{Transport: pod.client.Transport, Timeout: 200 * time.Millisecond}
	body := `{"workspace_id":"` + wsA + `","daemon_id":"pod-pool-0","device_name":"pool-0","runtimes":[{"name":"claude","type":"claude","version":"1","status":"online"}]}`
	req, _ := http.NewRequest(http.MethodPost, h.podAPI.URL+"/api/daemon/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pod.token)
	if resp, err := short.Do(req); err == nil { // the client gives up first
		resp.Body.Close()
	}
	eventually(t, "the visibility update to reach Multica", func() bool { return delivered.Load() })
	if n := up.pathCount("POST /api/daemon/runtimes/rt-" + wsA[:8] + "/recover-orphans"); n != 1 {
		t.Fatalf("orphan recovery calls: %d", n)
	}
}
