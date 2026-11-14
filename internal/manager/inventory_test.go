package manager_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// What the inventory tells an operator.
func TestInventoryReportsTheLatestSync(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	if inv := h.inventory(); inv.LastSync != nil {
		t.Fatalf("no sync has run: %+v", inv.LastSync)
	}
	h.pool.Sync(context.Background())
	if ls := h.inventory().LastSync; ls == nil || !ls.OK || ls.Error != "" || ls.At.IsZero() {
		t.Fatalf("after a good sync: %+v", ls)
	}
	up.setListStatus(http.StatusInternalServerError)
	h.pool.Sync(context.Background())
	ls := h.inventory().LastSync
	if ls == nil || ls.OK || ls.Error == "" {
		t.Fatalf("after a failed listing: %+v", ls)
	}
	if strings.Contains(ls.Error, "boom") {
		t.Fatalf("the response body leaked into the inventory: %q", ls.Error)
	}
	up.setListStatus(0)
	up.setWorkspaces()
	h.pool.Sync(context.Background())
	if ls := h.inventory().LastSync; ls == nil || ls.OK || !strings.Contains(ls.Error, "no workspace") {
		t.Fatalf("an empty listing is a failure: %+v", ls)
	}
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// Readiness follows the latest successful listing; liveness does not.
func TestReadyzFollowsTheLatestSuccessfulSync(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up, func(c *manager.Config) { c.SyncInterval = 10 * time.Second })
	clock := h.withClock()
	ready := func() int { s, _ := getStatus(t, h.podAPI.URL+"/readyz"); return s }
	alive := func() int { s, _ := getStatus(t, h.podAPI.URL+"/healthz"); return s }

	if s := ready(); s != http.StatusServiceUnavailable {
		t.Fatalf("before the first sync: readyz %d", s)
	}
	if _, why := getStatus(t, h.podAPI.URL+"/readyz"); why == "" {
		t.Fatal("the answer must say why")
	}
	h.pool.Sync(context.Background())
	if s := ready(); s != http.StatusOK {
		t.Fatalf("after a sync: readyz %d", s)
	}

	clock.advance(25 * time.Second)
	if s := ready(); s != http.StatusOK {
		t.Fatalf("within three intervals: readyz %d", s)
	}
	// An empty listing is a failed sync but does not make the manager unready
	// before the window ends.
	up.setWorkspaces()
	h.pool.Sync(context.Background())
	if s := ready(); s != http.StatusOK {
		t.Fatalf("one empty listing: readyz %d", s)
	}
	clock.advance(6 * time.Second)
	if s := ready(); s != http.StatusServiceUnavailable {
		t.Fatalf("no success for three intervals: readyz %d", s)
	}
	if s := alive(); s != http.StatusOK {
		t.Fatalf("liveness must not follow readiness: healthz %d", s)
	}
	up.setWorkspaces(wsA)
	h.pool.Sync(context.Background())
	if s := ready(); s != http.StatusOK {
		t.Fatalf("after recovering: readyz %d", s)
	}
}

func TestReadyzIsOKAfterTheFirstSync(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	if s, _ := getStatus(t, h.podAPI.URL+"/readyz"); s != http.StatusOK {
		t.Fatalf("after the first sync: readyz %d", s)
	}
}

func TestInventoryReportsLeaseAgeAndUpstreamHealth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, farHeartbeat)
	clock := h.withClock()
	if age := h.inventory().OldestLeaseAgeSeconds; age != 0 {
		t.Fatalf("no lease: age %d", age)
	}
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	clock.advance(90 * time.Second)
	if age := h.inventory().OldestLeaseAgeSeconds; age < 90 || age > 100 {
		t.Fatalf("oldest lease age %d, want about 90", age)
	}

	pod.heartbeat()
	eventually(t, "the upstream heartbeat to be recorded", func() bool {
		ws := h.inventory().Workspaces
		return len(ws) == 1 && ws[0].Upstream != nil && ws[0].Upstream.OK && ws[0].Upstream.Op == "heartbeat"
	})

	pod.report("t1", "complete")
	if age := h.inventory().OldestLeaseAgeSeconds; age != 0 {
		t.Fatalf("lease over: age %d", age)
	}
}

func TestInventoryReportsAFailedClaim(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/tasks/claim" {
			http.Error(w, "database is down", http.StatusInternalServerError)
			return true
		}
		return false
	})
	h.dispatch()
	ws := h.inventory().Workspaces
	if len(ws) != 1 || ws[0].Upstream == nil || ws[0].Upstream.OK || ws[0].Upstream.Op != "claim" || ws[0].Upstream.Error == "" {
		t.Fatalf("workspace upstream = %+v", ws)
	}
	if strings.Contains(ws[0].Upstream.Error, "database") {
		t.Fatalf("the response body leaked into the inventory: %q", ws[0].Upstream.Error)
	}
}

func TestPendingFailReportsShowInTheInventory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	if n := h.inventory().PendingFailReports; n != 0 {
		t.Fatalf("pending %d", n)
	}
}

// A reload that changes nothing is not worth an Info line.
func TestUnchangedSyncLogsNoReload(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA)
	h := newSyncHarness(t, up)
	const msg = "workspace configuration reloaded"
	h.pool.Sync(context.Background())
	if n := strings.Count(h.logs.String(), msg); n != 1 {
		t.Fatalf("the first sync adds a workspace and says so: %d lines\n%s", n, h.logs.String())
	}
	for range 3 {
		h.pool.Sync(context.Background())
	}
	if n := strings.Count(h.logs.String(), msg); n != 1 {
		t.Fatalf("unchanged syncs must stay quiet at Info: %d lines", n)
	}
	up.setWorkspaces(wsA, wsB)
	h.pool.Sync(context.Background())
	if n := strings.Count(h.logs.String(), msg); n != 2 {
		t.Fatalf("a real change is logged: %d lines", n)
	}
}
