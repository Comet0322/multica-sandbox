package dispatcher_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/dispatcher"
)

// The pods present the pod token their entrypoint got, as the stock daemon
// does from its config. A reset leaves the daemon running: the supervisor's
// report completes it.

func TestStockDaemonLeaseResetIdleCycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseAndComplete(t, h, pod, "t1")
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("want resetting after the task, got %+v", inv)
	}

	// The daemon keeps registering through the reset; that ends nothing.
	pod.startRaw()
	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("a registration before the supervisor reported ended the reset: %+v", inv)
	}
	h.upstream.enqueue("t2")
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("pod was lent before its reset was reported: %v", got)
	}

	cmd := pod.pollControl()
	if cmd == nil || cmd.Command != "reset" {
		t.Fatalf("want a reset command, got %+v", cmd)
	}
	if code := pod.reportReset(cmd.ID, "/home/daemon/stuck", "/home/daemon/also-stuck"); code != http.StatusOK {
		t.Fatalf("report done: status %d", code)
	}
	// The report alone, with paths left behind, returns the pod to the pool.
	if inv := h.inventory(); inv.Idle != 1 || inv.Resetting != 0 {
		t.Fatalf("want idle on the supervisor's report, got %+v", inv)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2 without the daemon registering again, got %v", got)
	}
	for _, want := range []string{"/home/daemon/stuck", "/home/daemon/also-stuck"} {
		if !strings.Contains(h.logs.String(), want) {
			t.Errorf("the path the reset could not delete is not logged: %s\n%s", want, h.logs.String())
		}
	}
}

func TestRegisteredPodIsResetBeforeItIsFirstLent(t *testing.T) {
	t.Parallel()
	// A pod that just registered is reset first, which also proves its
	// supervisor answers on the control channel.
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	h.upstream.enqueue("t1")
	pod.startRaw()

	if inv := h.inventory(); inv.Resetting != 1 || inv.Idle != 0 {
		t.Fatalf("an unseen pod must start in reset, got %+v", inv)
	}
	if got := pod.claimBatch(); len(got) != 0 {
		t.Fatalf("an unseen pod was handed %v", got)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("a task was claimed for an unseen pod")
	}
	cmd := pod.pollControl()
	if cmd == nil || cmd.Command != "reset" {
		t.Fatalf("the supervisor must be told to reset the pod, got %+v", cmd)
	}
	pod.reportControl(cmd.ID, "done")
	pod.startRaw()
	if inv := h.inventory(); inv.Idle != 1 {
		t.Fatalf("want idle once reset, got %+v", inv)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
}

func TestLostPodEndsItsLeases(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, shortTimeout)
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("pool-0")
	healthy := h.newPod("pool-1")
	pod.start()
	healthy.start()
	if task := pod.claimTask(); task == nil {
		t.Fatal("the pod got no task")
	}

	time.Sleep(3500 * time.Millisecond)
	h.inventory() // any call into the sandbox service notices the silence

	eventually(t, "task failed upstream", func() bool {
		msg, ok := h.upstream.failureOf("t1")
		return ok && strings.Contains(msg, "lost")
	})
	ls := h.leases()
	if len(ls) != 1 || ls[0].EndedAt == nil || ls[0].EndReason != "pod lost" {
		t.Fatalf("lease should be closed with the reason, got %+v", ls)
	}
	var released bool
	for _, r := range h.auditRecords() {
		released = released || (r.Action == dispatcher.AuditRelease && r.TaskID == "t1" && r.Reason == "pod lost")
	}
	if !released {
		t.Fatalf("no release audit for the lost pod: %+v", h.auditRecords())
	}

	// If it comes back it carries the dead task's files: it is reset first.
	pod.pollControl()
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("a lost pod that returns must be reset before it is lent, got %+v", inv)
	}
}

func TestResetIsAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseAndComplete(t, h, pod, "t1")
	cmd := pod.pollControl()
	pod.reportControl(cmd.ID, "done")
	pod.startRaw()

	var got []string
	for _, r := range h.auditRecords() {
		if r.Action != dispatcher.AuditReset || r.TaskID != "t1" {
			continue
		}
		if r.Decision != dispatcher.AuditAllow || r.Initiator != "alice@example.com" || r.PodID != pod.id || r.WorkspaceID != harnessWorkspaceID {
			t.Fatalf("incomplete reset record: %+v", r)
		}
		got = append(got, strings.SplitN(r.Reason, ":", 2)[0])
	}
	if !slices.Equal(got, []string{"reset started", "reset completed"}) {
		t.Fatalf("want a start and a completion record for t1, got %v", got)
	}
}

func TestResetTimeoutAndLeftoversAreAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, shortReset)
	timedOut := h.newPod("pool-0")
	partial := h.newPod("pool-1")
	partial.start()
	leaseAndComplete(t, h, timedOut, "t1")
	h.upstream.enqueue("t2")
	if got := partial.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2, got %v", got)
	}
	partial.report("t2", "complete")
	cmd := partial.pollControl()
	partial.reportReset(cmd.ID, "/home/daemon/a", "/home/daemon/b")

	time.Sleep(3500 * time.Millisecond)
	h.inventory()

	deny, allow := map[string]string{}, map[string][]string{}
	for _, r := range h.auditRecords() {
		if r.Action != dispatcher.AuditReset {
			continue
		}
		if r.Decision == dispatcher.AuditDeny {
			deny[r.PodID] = r.Reason
		} else {
			allow[r.PodID] = append(allow[r.PodID], r.Reason)
		}
	}
	if !strings.Contains(deny[timedOut.id], "timed out") {
		t.Fatalf("want a timeout record for the silent pod, got %v", deny)
	}
	if _, ok := deny[partial.id]; ok || !slices.Contains(allow[partial.id], "reset completed; 2 paths could not be deleted") {
		t.Fatalf("want a completion record counting the leftovers, got allow %v deny %v", allow, deny)
	}
}

func TestLeasesAndInventoryNameTheInitiators(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	pod := h.newPod("pool-0")
	pod.start()
	pod.claimBatch()

	ls := h.leases()
	if len(ls) != 1 || ls[0].Initiator != "alice@example.com" {
		t.Fatalf("lease should name its initiator, got %+v", ls)
	}
	inv := h.inventory()
	if len(inv.Pods) != 1 || !slices.Equal(inv.Pods[0].Initiators, []string{"alice@example.com"}) {
		t.Fatalf("pod should list the initiators it serves, got %+v", inv.Pods)
	}
}
