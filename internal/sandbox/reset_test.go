package sandbox_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

func shortReset(c *sandbox.Config) { c.ResetTimeout = 3 * time.Second }

func TestAResetNotReportedInTimeMakesThePodLostUntilItIsResetAgain(t *testing.T) {
	h := newHarness(t, shortReset)
	pod := h.registerClean("pool-0")
	h.delete(h.mustCreate())
	cmd := pod.poll()

	h.advance(3500 * time.Millisecond)
	h.wantState(pod, sandbox.PodLost)
	h.wantNoCapacity()

	// The late report of the timed-out reset does not free the pod: the pod
	// speaking again brings it back through a new reset.
	pod.report(cmd.ID)
	h.wantState(pod, sandbox.PodResetting)
	h.wantNoCapacity()
	again := pod.poll()
	if again == nil || again.ID == cmd.ID {
		t.Fatalf("want a new reset command, got %+v", again)
	}
	pod.report(again.ID)
	if got := h.mustCreate(); got != pod.id {
		t.Fatalf("create returned %q, want %q", got, pod.id)
	}
}

// Only the reset timeout bounds a reset, so silence during one does not lose
// the pod.
func TestAResetStaysWithinItsTimeoutWhileSilent(t *testing.T) {
	h := newHarness(t, func(c *sandbox.Config) {
		c.HeartbeatTimeout = time.Second
		c.ResetTimeout = 10 * time.Second
	})
	pod := h.register("pool-0")
	h.advance(5 * time.Second)
	h.wantState(pod, sandbox.PodResetting)
}

func TestAResetTimeoutOfAHandedBackPodTellsNoOne(t *testing.T) {
	h := newHarness(t, shortReset)
	h.registerClean("pool-0")
	h.delete(h.mustCreate())
	h.advance(3500 * time.Millisecond)
	h.svc.Inventory()
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("a pod nobody holds was reported: %+v", got)
	}
}
