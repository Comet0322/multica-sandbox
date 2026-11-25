package manager_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// A listing that loses a workspace once must not drain it.
func TestSyncRemovesAWorkspaceOnlyAfterTwoMissesInARow(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	served := func() []string { return workspaceIDs(h.inventory().Workspaces) }
	sync := func() manager.ReloadResult {
		t.Helper()
		res, err := h.pool.Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	sync()

	up.setWorkspaces(wsA)
	if res := sync(); len(res.Removed) != 0 || !slices.Equal(served(), []string{wsA, wsB}) {
		t.Fatalf("first miss: %+v, serving %v", res, served())
	}
	if !strings.Contains(h.logs.String(), "missing from the listing") {
		t.Fatalf("the kept workspace must be logged:\n%s", h.logs.String())
	}

	// It is listed again: the count starts over.
	up.setWorkspaces(wsA, wsB)
	sync()
	up.setWorkspaces(wsA)
	if res := sync(); len(res.Removed) != 0 || !slices.Equal(served(), []string{wsA, wsB}) {
		t.Fatalf("a miss after a hit counts as the first again: %+v, serving %v", res, served())
	}
	if res := sync(); !slices.Equal(res.Removed, []string{wsB}) || !slices.Equal(served(), []string{wsA}) {
		t.Fatalf("second miss in a row: %+v, serving %v", res, served())
	}
}

// A failed listing does not count as a miss.
func TestSyncFailedListingsAreNotMisses(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	up.setWorkspaces(wsA)
	h.pool.Sync(context.Background()) // first miss
	up.setListStatus(http.StatusInternalServerError)
	for range 3 {
		h.pool.Sync(context.Background())
	}
	up.setListStatus(0)
	up.setWorkspaces(wsA, wsB)
	h.pool.Sync(context.Background())
	if got := workspaceIDs(h.inventory().Workspaces); !slices.Equal(got, []string{wsA, wsB}) {
		t.Fatalf("serving %v", got)
	}
}

// A workspace back before its drain ended is the same object: its
// registration and its running tasks carry on, and the drain stops.
func TestWorkspaceAddedBackWhileDrainingKeepsItsLeasesAndRegistration(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, 250*time.Millisecond)
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	up.enqueue(a, "ta1")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want ta1, got %v", got)
	}
	registers := up.callCount(a, "POST /api/daemon/register")

	if res, err := h.pool.Reload(context.Background(), all[1:]); err != nil || !slices.Equal(res.Removed, []string{wsA}) {
		t.Fatalf("remove A: %+v, %v", res, err)
	}
	if got := h.inventory().Draining; len(got) != 1 {
		t.Fatalf("A must be draining: %+v", got)
	}
	res, err := h.pool.Reload(context.Background(), all)
	if err != nil || !slices.Equal(res.Added, []string{wsA}) {
		t.Fatalf("add A back: %+v, %v", res, err)
	}
	inv := h.inventory()
	if len(inv.Draining) != 0 {
		t.Fatalf("A is served again, it cannot still be draining: %+v", inv.Draining)
	}
	var leased int
	for _, w := range inv.Workspaces {
		if w.WorkspaceID == wsA {
			leased = w.Leased
		}
	}
	if leased != 1 {
		t.Fatalf("A must keep its lease: %+v", inv.Workspaces)
	}

	// The drain of the first removal would have ended the lease by now.
	time.Sleep(600 * time.Millisecond)
	h.wantHeld("ta1")
	if reports := up.reportsOf(a); slices.Contains(reports, "ta1/fail") {
		t.Fatalf("the running task was failed: %v", reports)
	}

	// The daemon registers again: no new upstream registration.
	pod.registerFor(wsA)
	if n := up.callCount(a, "POST /api/daemon/register"); n != registers {
		t.Fatalf("A registered upstream again: %d -> %d", registers, n)
	}
	if status := pod.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	h.wantEnded("ta1", "pod reported complete")
}

func TestRemovedAddedAndRemovedAgainDrainsOnce(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	all := twoWorkspaces(nil, nil)
	h := newReloadHarness(t, up, all, 300*time.Millisecond)
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	up.enqueue(a, "ta1")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want ta1, got %v", got)
	}

	ctx := context.Background()
	h.pool.Reload(ctx, all[1:])
	h.pool.Reload(ctx, all)
	h.pool.Reload(ctx, all[1:])

	awaitReload(t, "the drain to give up", func() bool { return len(h.inventory().Draining) == 0 })
	h.wantEnded("ta1", "removed")
	time.Sleep(200 * time.Millisecond) // a stale drain would act now
	fails := 0
	for _, r := range up.reportsOf(a) {
		if r == "ta1/fail" {
			fails++
		}
	}
	if fails != 1 {
		t.Fatalf("the abandoned task must be failed once, got %d", fails)
	}
}
