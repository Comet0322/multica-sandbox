package sandbox_test

import (
	"net/http"
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

// A pod the service has never seen may hold anything: it is reset before
// Create can hand it out.
func TestANewPodIsResetBeforeCreateReturnsIt(t *testing.T) {
	h := newHarness(t)
	pod := h.register("pool-0")

	h.wantState(pod, sandbox.PodResetting)
	h.wantNoCapacity()

	pod.completeReset()
	h.wantState(pod, sandbox.PodFree)
	if got := h.mustCreate(); got != pod.id {
		t.Fatalf("create returned %q, want %q", got, pod.id)
	}
	h.wantState(pod, sandbox.PodHeld)
}

func TestCreateNeverReturnsAHeldPodAndRunsOutOfCapacity(t *testing.T) {
	h := newHarness(t)
	a, b := h.registerClean("pool-0"), h.registerClean("pool-1")

	first, second := h.mustCreate(), h.mustCreate()
	if first != a.id || second != b.id {
		t.Fatalf("create returned %q then %q, want %q then %q", first, second, a.id, b.id)
	}
	h.wantNoCapacity()
}

func TestAfterDeleteCreateWaitsForTheResetToBeReportedDone(t *testing.T) {
	h := newHarness(t)
	pod := h.registerClean("pool-0")
	id := h.mustCreate()

	h.delete(id)
	h.wantState(pod, sandbox.PodResetting)
	h.wantNoCapacity()
	if cmd := pod.poll(); cmd == nil || cmd.Command != "reset" {
		t.Fatalf("want a reset command after delete, got %+v", cmd)
	}
	// Taking the command is not finishing it.
	h.wantNoCapacity()

	pod.completeReset()
	if got := h.mustCreate(); got != pod.id {
		t.Fatalf("create returned %q, want the reset pod %q", got, pod.id)
	}
}

func TestAStaleOrForeignResetReportChangesNothing(t *testing.T) {
	h := newHarness(t)
	a := h.register("pool-0")
	b := h.registerClean("pool-1")
	cmd := a.poll()

	if status := b.report(cmd.ID); status != http.StatusOK {
		t.Fatalf("foreign report: status %d, want it acknowledged", status)
	}
	if status := a.report("some-other-reset"); status != http.StatusOK {
		t.Fatalf("stale report: status %d, want it acknowledged", status)
	}
	h.wantState(a, sandbox.PodResetting)
	if again := a.poll(); again == nil || again.ID != cmd.ID {
		t.Fatalf("the pending command must be offered until reported, got %+v", again)
	}
	if got := b.poll(); got != nil {
		t.Fatalf("pod b was offered a command: %+v", got)
	}
}

func TestDeleteOfALostOrUnknownPodDoesNothing(t *testing.T) {
	h := newHarness(t)
	pod := h.registerClean("pool-0")
	h.advance(sandbox.DefaultHeartbeatTimeout + time.Second)
	h.wantState(pod, sandbox.PodLost)

	h.delete(pod.id)
	h.delete("pod-unknown")
	h.wantState(pod, sandbox.PodLost)
	if n := len(h.svc.Inventory().Pods); n != 1 {
		t.Fatalf("want the one lost pod listed, got %d pods", n)
	}
}

func TestFilesAndExecAreUnsupported(t *testing.T) {
	h := newHarness(t)
	pod := h.registerClean("pool-0")
	id := h.mustCreate()
	ctx := t.Context()

	for _, p := range []sandbox.PodID{id, "pod-unknown"} {
		if err := h.svc.PutFiles(ctx, p, "/home/pod", nil); err != sandbox.ErrUnsupported {
			t.Errorf("PutFiles(%s) = %v", p, err)
		}
		if rc, err := h.svc.GetFiles(ctx, p, "/home/pod"); err != sandbox.ErrUnsupported || rc != nil {
			t.Errorf("GetFiles(%s) = %v, %v", p, rc, err)
		}
		if _, err := h.svc.Exec(ctx, p, sandbox.Command{Argv: []string{"true"}}); err != sandbox.ErrUnsupported {
			t.Errorf("Exec(%s) = %v", p, err)
		}
	}
	h.wantState(pod, sandbox.PodHeld)
}
