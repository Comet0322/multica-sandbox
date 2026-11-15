package manager_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// The manager keeps its leases in memory. A task that was running when the
// manager restarted is gone from the registry, its pod is reset, and nothing
// would ever finish it: Multica kept it "running" until its own timeout (hours).
// The first time the restarted manager registers the pool's runtime it asks
// Multica to fail what is still running on it (the same call a restarted
// daemon makes), which also lets Multica retry the task.
func TestFirstRegistrationAsksMulticaToRecoverOrphanedTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	const call = "POST /api/daemon/runtimes/runtime-a/recover-orphans"

	h.newPod("pool-0").start()
	deadline := time.Now().Add(2 * time.Second)
	for h.upstream.callsTo(call) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.upstream.callsTo(call); got != 1 {
		t.Fatalf("recover-orphans calls after the first pod registered: want 1, got %d", got)
	}

	// Pods that join later, or register again, must not fail tasks that are
	// running on their neighbours.
	h.newPod("pool-1").start()
	time.Sleep(200 * time.Millisecond)
	if got := h.upstream.callsTo(call); got != 1 {
		t.Fatalf("recover-orphans calls after a second pod registered: want 1, got %d", got)
	}
}

// Orphans are recovered only for a workspace that holds nothing here.
func TestOrphanRecoveryIsSkippedForAWorkspaceWithAnActiveLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	recover := "POST /api/daemon/runtimes/runtime-a/recover-orphans"

	// The first registration finds nothing running here: orphans are
	// recovered, after the registration and before any claim.
	var order []string
	var claimsAtRecovery int
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		h.upstream.mu.Lock()
		defer h.upstream.mu.Unlock()
		order = append(order, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/daemon/runtimes/runtime-a/recover-orphans" {
			claimsAtRecovery = h.upstream.claims
		}
		return false
	})
	leaseOne(t, h, pod, "t1")
	if n := h.upstream.callCount(recover); n != 1 {
		t.Fatalf("recover-orphans calls at first registration: %d", n)
	}
	h.upstream.mu.Lock()
	reg, rec := slices.Index(order, "POST /api/daemon/register"), slices.Index(order, recover)
	claims := claimsAtRecovery
	h.upstream.mu.Unlock()
	if reg < 0 || rec < reg {
		t.Fatalf("recover-orphans must follow the registration: %v", order)
	}
	if claims != 0 {
		t.Fatalf("a task was claimed before orphans were recovered: %d claims", claims)
	}

	// The runtime is deleted upstream while t1 runs; the registration is
	// dropped and made again with a live lease in the workspace.
	var gone atomic.Bool
	gone.Store(true)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/heartbeat" && gone.Load() {
			http.Error(w, runtimeGoneBody, http.StatusNotFound)
			return true
		}
		return false
	})
	registered := h.upstream.registrationCount()
	h.pool.HeartbeatOnce(context.Background())
	eventually(t, "the registration to be dropped", func() bool { return pod.heartbeat() == http.StatusNotFound })
	gone.Store(false)
	pod.register()
	if n := h.upstream.registrationCount(); n != registered+1 {
		t.Fatalf("the workspace did not register again: %d -> %d", registered, n)
	}
	if n := h.upstream.callCount(recover); n != 1 {
		t.Fatalf("orphans were recovered under a live lease (%d calls): t1 would have been failed", n)
	}
	h.wantHeld("t1")
}

// A restarted manager fails what Multica still shows running on its runtimes
// in every served workspace once, on its first Sync, without waiting for a pod
// to register. Runtimes of other daemons are left alone.
func TestFirstSyncRecoversTheOrphansOfTheManagersRuntimes(t *testing.T) {
	t.Parallel()
	up := newFakeMultica(t)
	var listedFor atomic.Value
	up.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/api/runtimes":
			listedFor.Store(r.Header.Get("X-Workspace-ID"))
			writeJSON(w, []map[string]any{
				{"id": "rt-own", "daemon_id": "company-sandbox-prod", "visibility": "public"},
				{"id": "rt-member", "daemon_id": "alice-laptop", "visibility": "private"},
			})
			return true
		case strings.HasSuffix(r.URL.Path, "/recover-orphans"):
			writeJSON(w, map[string]any{"orphaned": 1, "retried": 1})
			return true
		}
		return false
	})
	pool, err := manager.New(manager.Config{
		MulticaURL: up.URL, PAT: harnessPAT, Name: "Test Pool", DaemonID: "company-sandbox-prod",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := pool.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := up.callCount("POST /api/daemon/runtimes/rt-own/recover-orphans"); got != 1 {
		t.Fatalf("recover-orphans on the manager's runtime: want 1 call, got %d", got)
	}
	if got := up.callCount("POST /api/daemon/runtimes/rt-member/recover-orphans"); got != 0 {
		t.Fatalf("recover-orphans on another daemon's runtime: want none, got %d", got)
	}
	if got, _ := listedFor.Load().(string); got != harnessWorkspaceID {
		t.Fatalf("runtimes listed for workspace %q, want %q", got, harnessWorkspaceID)
	}
}
