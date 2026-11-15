package manager_test

import (
	"net/http"
	"slices"
	"testing"

	"sandbox-manager/internal/manager"
)

// The pods present the pod token the manager put in their environment, as
// the stock daemon does from its config.

// A pod is starting until its daemon first calls the manager, and nothing is
// claimed for it before.
func TestAPodIsReadyOnceItsDaemonCallsIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	h.upstream.enqueue("t1")
	if inv := h.inventory(); inv.Starting != 1 || inv.Warm != 0 {
		t.Fatalf("a pod whose daemon has not called must be starting, got %+v", inv)
	}
	h.dispatch()
	if h.upstream.queued() != 1 {
		t.Fatal("a task was claimed for a pod that is not ready")
	}
	pod.start()
	if inv := h.inventory(); inv.Warm != 1 {
		t.Fatalf("want warm once its daemon called in, got %+v", inv)
	}
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}
}

func TestLostPodEndsItsLeases(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	h.upstream.enqueueTask("t1", "alice@example.com", nil)
	pod := h.newPod("pool-0")
	healthy := h.newPod("pool-1")
	pod.start()
	healthy.start()
	if task := pod.claimTask(); task == nil {
		t.Fatal("the pod got no task")
	}

	h.loseDaemon(pod, "t1")
	if msg, _ := h.upstream.failureOf("t1"); msg != "the pod running this task was lost: "+daemonExited {
		t.Fatalf("t1 failed with %q", msg)
	}
	ls := h.leases()
	if len(ls) != 1 || ls[0].EndedAt == nil || ls[0].EndReason != "pod lost" {
		t.Fatalf("lease should be closed with the reason, got %+v", ls)
	}
	var released bool
	for _, r := range h.auditRecords() {
		released = released || (r.Action == manager.AuditRelease && r.TaskID == "t1" && r.Reason == "pod lost")
	}
	if !released {
		t.Fatalf("no release audit for the lost pod: %+v", h.auditRecords())
	}
	for _, p := range h.inventory().Pods {
		if p.ID == pod.id && (p.Phase != manager.PodFailed || p.Reason != daemonExited) {
			t.Fatalf("the lost pod: %+v, want failed with its reason", p)
		}
	}
	// It is deleted, and its token no longer works.
	eventually(t, "the lost pod deleted", func() bool { return !h.kubeHas(pod.id) })
	if code := pod.heartbeat(); code != http.StatusUnauthorized {
		t.Fatalf("heartbeat of a lost pod: %d, want 401", code)
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
