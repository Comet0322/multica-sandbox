package manager_test

// These tests check that the manager carries out the lending policy's hold,
// lifetime and eviction decisions: the reset sent, the task failed upstream,
// the audit record written. The decisions themselves are tested in lending.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// lifetimes sets the hold timeout, maximum lifetime and drain grace.
func lifetimes(hold, life, grace time.Duration) func(*manager.Config) {
	return func(c *manager.Config) {
		c.HoldTimeout = hold
		c.MaxLifetime = life
		c.DrainGrace = grace
	}
}

// shared combines shared placement with lifetime settings.
func shared(capacity int, opts ...func(*manager.Config)) func(*manager.Config) {
	return func(c *manager.Config) {
		sharedPods(capacity, 0)(c)
		for _, opt := range opts {
			opt(c)
		}
	}
}

func TestHeldPodIsResetWhenItsHoldRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(150*time.Millisecond, time.Hour, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	time.Sleep(300 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("a pod whose hold ran out should be resetting: %+v", inv)
	}
	h.upstream.enqueueIn("t2", "issue-1", "")
	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("a resetting pod took a new task: %v", got)
	}
	a.completeReset()
	// The binding is gone: the same issue's next task binds a pod anew.
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the next task of the key should bind a pod again: %v", got)
	}
	if got := leasePods(h); got["t2"] != a.id {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestSharedPodStillRunningAfterTheGraceIsReleasedAndItsTasksFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(time.Hour, 100*time.Millisecond, 150*time.Millisecond)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()

	time.Sleep(400 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the pod should be released after the grace: %+v", inv)
	}
	a.heartbeat()
	why, ok := h.upstream.failureOf("t1")
	if !ok || !strings.Contains(why, "lifetime") {
		t.Fatalf("the unfinished task should be failed with the reason, got %q (%v)", why, ok)
	}
	if got := leasePods(h); len(got) != 0 {
		t.Fatalf("leases should be closed: %v", got)
	}
}

func TestHeldPodIsEvictedForATaskOfAnotherKeyWhenNoPodIsFree(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(time.Hour, 2*time.Hour, time.Hour)))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	// The only pod holds issue-1 with nothing running; issue-2 must not wait
	// for the hold to run out.
	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("the evicted pod must be reset before it runs the task: %v", got)
	}
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the held pod should be reset for the other key's task: %+v", inv)
	}
	var evicted bool
	for _, r := range h.auditRecords() {
		if r.Action == manager.AuditReset && r.PodID == a.id && strings.Contains(r.Reason, "evicted for task t2") {
			evicted = true
		}
	}
	if !evicted {
		t.Fatalf("the eviction is not in the audit log: %+v", h.auditRecords())
	}
	a.completeReset()
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the task should run on the evicted pod: %v", got)
	}
}

// A pod lost in hold leaves the audit log saying so, and is reset before it is
// lent again.
func TestLostHeldPodIsAuditedAndResetWhenItReturns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, shared(3, lifetimes(time.Hour, 2*time.Hour, time.Hour)), func(c *manager.Config) {
		c.HeartbeatTimeout = 300 * time.Millisecond
	})
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	a.report("t1", "complete")

	time.Sleep(500 * time.Millisecond)
	if inv := h.inventory(); inv.Lost != 1 {
		t.Fatalf("the silent pod should be lost: %+v", inv)
	}
	var audited bool
	for _, r := range h.auditRecords() {
		if r.Action == manager.AuditReset && r.PodID == a.id && strings.HasPrefix(r.Reason, "pod lost") {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("the lost pod is not in the audit log: %+v", h.auditRecords())
	}
	a.pollControl() // the supervisor speaks again
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("a pod lost in hold must be reset before it is lent again: %+v", inv)
	}
}

func TestNegativeLifetimeConfigIsRejected(t *testing.T) {
	t.Parallel()
	for name, opt := range map[string]func(*manager.Config){
		"hold":  func(c *manager.Config) { c.HoldTimeout = -time.Second },
		"life":  func(c *manager.Config) { c.MaxLifetime = -time.Second },
		"grace": func(c *manager.Config) { c.DrainGrace = -time.Second },
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
