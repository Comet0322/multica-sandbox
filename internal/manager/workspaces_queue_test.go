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

func TestPriorityDoesNotCrossWorkspaces(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	created := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	// B's task is more urgent within B, but A's has waited longer and both
	// workspaces occupy nothing: priority does not let B jump ahead.
	up.enqueueRanked(b, "b-new-high", 3, created.Add(time.Minute))
	up.enqueueRanked(a, "a-old-low", 1, created)

	h.dispatch()

	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"a-old-low"}) {
		t.Fatalf("want the longer-waiting task of A, got %v", got)
	}
	if n := up.claimCount(b); n != 0 {
		t.Fatalf("no claim may reach B before A's longer-waiting task, got %d", n)
	}
}
