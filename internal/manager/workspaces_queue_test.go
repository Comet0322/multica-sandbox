package manager_test

import (
	"slices"
	"testing"
	"time"
)

func TestDispatchTickReadsOnePendingListPerWorkspaceRuntime(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-0")
	pod.startAll()
	beforeA, beforeB := up.pendingReads(a), up.pendingReads(b)

	h.dispatch()
	h.dispatch()

	if got := up.pendingReads(a) - beforeA; got != 2 {
		t.Fatalf("workspace A: want one pending read per tick (2), got %d", got)
	}
	if got := up.pendingReads(b) - beforeB; got != 2 {
		t.Fatalf("workspace B: want one pending read per tick (2), got %d", got)
	}
}

func TestClaimGoesToTheWorkspaceWithTheHighestPriorityPendingTask(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	created := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	// A comes first in turn and holds the older task; B's is more urgent.
	up.enqueueRanked(a, "a-old-low", 1, created)
	up.enqueueRanked(b, "b-new-high", 3, created.Add(time.Minute))

	h.dispatch()

	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"b-new-high"}) {
		t.Fatalf("want the higher-priority task of B, got %v", got)
	}
	if n := up.claimCount(a); n != 0 {
		t.Fatalf("no claim may reach A before B's more urgent task, got %d", n)
	}
}
