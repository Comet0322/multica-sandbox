package manager_test

// These tests check that the manager carries out the policy's hold and
// budget decisions on its pods: the pod made idle or deleted, the audit
// record written. The decisions themselves are tested in internal/policy.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

func TestAPoolPodIsIdleWhenItsHoldRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(150*time.Millisecond))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")
	if inv := h.inventory(); inv.Held != 1 {
		t.Fatalf("a pod whose task ended should be held: %+v", inv)
	}

	time.Sleep(300 * time.Millisecond)
	if inv := h.inventory(); inv.Idle != 1 || inv.Held != 0 {
		t.Fatalf("a pool runtime pod whose hold ran out should be idle: %+v", inv)
	}
	// Another key of the workspace takes it.
	h.upstream.enqueueIn("t2", "issue-2", "")
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("another key's task should take the idle pod: %v", got)
	}
}

func TestACleanPodIsDeletedWhenItsHoldRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(150*time.Millisecond), clean)
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	time.Sleep(300 * time.Millisecond)
	h.inventory()
	eventually(t, "the clean pod deleted", func() bool { return !h.kubeHas(a.id) })
	if !deletedFor(h, a.id, "hold expired") {
		t.Fatalf("the deletion is not in the audit log: %+v", h.auditRecords())
	}
	// The next task of the key gets a new pod.
	h.upstream.enqueueIn("t2", "issue-1", "")
	b := h.newPod("pool-1")
	b.start()
	if got := b.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the key's next task should take a new pod: %v", got)
	}
}

func TestAHeldPodIsDeletedForTheBudgetWhenNoPodIsFree(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	// The only pod is held for issue-1 with nothing running; issue-2 must
	// not wait for the hold to run out.
	h.dispatch()
	if !deletedFor(h, a.id, "the pod budget is full") {
		t.Fatalf("the deletion is not in the audit log: %+v", h.auditRecords())
	}
	b := h.newPod("pool-1")
	b.start()
	if got := b.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the task should run on the pod created in its place: %v", got)
	}
}

// deletedFor reports whether the audit log records the pod's deletion with a
// reason containing why.
func deletedFor(h *harness, pod, why string) bool {
	for _, r := range h.auditRecords() {
		if r.Action == "delete" && r.PodID == pod && strings.Contains(r.Reason, why) {
			return true
		}
	}
	return false
}

func TestNegativeLifetimeConfigIsRejected(t *testing.T) {
	t.Parallel()
	for name, opt := range map[string]func(*manager.Config){
		"hold": func(c *manager.Config) { c.HoldTimeout = -time.Second },
		"idle": func(c *manager.Config) { c.PodMaxIdle = -time.Second },
	} {
		cfg := manager.Config{
			MulticaURL: "http://127.0.0.1:1", PAT: harnessPAT,
		}
		opt(&cfg)
		if _, err := manager.New(cfg, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
