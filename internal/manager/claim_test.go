package manager_test

import (
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

func TestNoClaimWithoutIdlePodPolling(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")

	// Ten poll periods with no pod asking: the manager never goes upstream.
	time.Sleep(10 * 100 * time.Millisecond)
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("want 0 claims with no polling pod, got %d", n)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("task left the upstream queue")
	}
}

func TestIdlePodPollingGetsExactlyOneClaimedTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	pod := h.newPod("pool-0")
	pod.start()

	began := time.Now()
	got := pod.claimBatch()
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("handing the task to the pod took %s", elapsed)
	}
	if !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want [t1], got %v", got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want exactly 1 upstream claim, got %d", n)
	}
	if inv := h.inventory(); inv.Running != 1 || inv.Warm != 0 {
		t.Fatalf("want the pod running, got %+v", inv)
	}
}

func TestTwoIdlePodsTwoTasksEachGetOneWithoutDuplicates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	a := h.newPod("pool-0")
	b := h.newPod("pool-1")
	a.start()
	b.start()

	// Only the dispatch tick claims, one task per tick, and a task may land
	// on either pod; the two ticks run first so both polls race over placed
	// tasks, as two daemons polling at once do.
	h.dispatch()
	h.dispatch()
	var wg sync.WaitGroup
	var gotA, gotB []string
	wg.Add(2)
	go func() { defer wg.Done(); gotA = a.pollBatch() }()
	go func() { defer wg.Done(); gotB = b.pollBatch() }()
	wg.Wait()

	if len(gotA) != 1 || len(gotB) != 1 || gotA[0] == gotB[0] {
		t.Fatalf("want one distinct task each, got %v and %v", gotA, gotB)
	}
	if inv := h.inventory(); inv.Running != 2 {
		t.Fatalf("want 2 running, got %+v", inv)
	}
}

func TestLeasedPodCannotClaimASecondTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	pod := h.newPod("pool-0")
	pod.start()
	pod.claimBatch()
	pod.report("t1", "start")

	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("leased pod was handed %v", got)
	}
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want 1 upstream claim, got %d", n)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("second task should still be queued upstream")
	}
}

func TestPodReportsAreForwardedAndForeignTasksRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueue("t1")
	owner := h.newPod("pool-0")
	other := h.newPod("pool-1")
	owner.start()
	other.start()
	owner.claimBatch()

	if status := other.report("t1", "progress"); status != http.StatusForbidden {
		t.Fatalf("foreign report: want 403, got %d", status)
	}
	if status := owner.report("unclaimed", "start"); status != http.StatusForbidden {
		t.Fatalf("unknown task report: want 403, got %d", status)
	}
	for _, action := range []string{"start", "progress"} {
		if status := owner.report("t1", action); status != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", action, status)
		}
	}
	want := []string{"t1 start", "t1 progress"}
	if got := h.upstream.taskCallsSeen(); !slices.Equal(got, want) {
		t.Fatalf("upstream saw %v, want %v", got, want)
	}
}

func TestLeaseIsRecordedAndReleasedOnCompleteOrFail(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueue("t1")
	h.upstream.enqueue("t2")
	a := h.newPod("pool-0")
	b := h.newPod("pool-1")
	a.start()
	b.start()
	a.claimBatch()
	b.claimBatch()

	byTask := map[string]manager.LeaseInfo{}
	for _, l := range h.leases() {
		byTask[l.TaskID] = l
	}
	if l := byTask["t1"]; l.PodID != a.id || l.StartedAt.IsZero() || l.EndedAt != nil {
		t.Fatalf("bad active lease for t1: %+v", l)
	}
	if l := byTask["t2"]; l.PodID != b.id {
		t.Fatalf("bad lease for t2: %+v", l)
	}

	if status := b.report("t2", "fail"); status != http.StatusOK {
		t.Fatalf("fail: status %d", status)
	}
	if status := a.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	for _, l := range h.leases() {
		if l.EndedAt == nil || l.EndedAt.Before(l.StartedAt) {
			t.Fatalf("lease %s not closed: %+v", l.TaskID, l)
		}
	}
	if inv := h.inventory(); inv.Running != 0 || inv.Idle != 2 {
		t.Fatalf("want both pods idle, got %+v", inv)
	}
	// An idle pod takes new work of its workspace, the one idle the shortest
	// time first, and the old task is no longer its own.
	h.upstream.enqueue("t3")
	if got := a.claimBatch(); !slices.Equal(got, []string{"t3"}) {
		t.Fatalf("want t3 after release, got %v", got)
	}
	if status := a.report("t1", "progress"); status != http.StatusForbidden {
		t.Fatalf("report on a released task: want 403, got %d", status)
	}
}
