package sandbox_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

// A daemon restarted by the supervisor presents the same pod token; the
// supervisor's event says what happened instead.
func TestARestartEventOnAHeldPodInterruptsItOnceAndResetsIt(t *testing.T) {
	for event, why := range map[string]string{
		"daemon-exited": "pod daemon restarted",
		"started":       "pod container restarted",
	} {
		t.Run(event, func(t *testing.T) {
			h := newHarness(t)
			pod := h.registerClean("pool-0")
			h.mustCreate()

			if status := pod.event(event); status != http.StatusOK {
				t.Fatalf("event: status %d", status)
			}
			want := []notice{{"interrupted", pod.id, why}}
			if got := h.rec.takeNotices(); !slices.Equal(got, want) {
				t.Fatalf("notices = %+v, want %+v", got, want)
			}
			h.wantState(pod, sandbox.PodResetting)
			h.wantNoCapacity()

			// The supervisor resends an event it is not sure was delivered.
			pod.event(event)
			if got := h.rec.takeNotices(); len(got) != 0 {
				t.Fatalf("the interruption was reported again: %+v", got)
			}
			pod.completeReset()
			if got := h.mustCreate(); got != pod.id {
				t.Fatalf("create returned %q, want %q", got, pod.id)
			}
		})
	}
}

func TestEventsLeaveAPodNobodyHoldsAlone(t *testing.T) {
	h := newHarness(t, shortHeartbeat)
	free := h.registerClean("pool-0")
	resetting := h.register("pool-1")
	resetCmd := resetting.poll()

	for _, ev := range []string{"started", "daemon-exited", "something-new"} {
		if status := free.event(ev); status != http.StatusOK {
			t.Fatalf("free %s: status %d", ev, status)
		}
		if status := resetting.event(ev); status != http.StatusOK {
			t.Fatalf("resetting %s: status %d", ev, status)
		}
	}
	h.wantState(free, sandbox.PodFree)
	h.wantState(resetting, sandbox.PodResetting)
	if again := resetting.poll(); again == nil || again.ID != resetCmd.ID {
		t.Fatalf("an event restarted the pending reset: %+v then %+v", resetCmd, again)
	}
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("pods nobody holds were reported: %+v", got)
	}
}

// An event from a lost pod is the pod speaking again: it comes back through a
// reset, and no one holds it to be told.
func TestAnEventFromALostPodBringsItBackThroughAReset(t *testing.T) {
	h := newHarness(t, shortHeartbeat)
	pod := h.registerClean("pool-0")
	h.mustCreate()
	h.advance(3500 * time.Millisecond)
	h.wantState(pod, sandbox.PodLost)
	h.rec.takeNotices()

	pod.event("daemon-exited")
	h.wantState(pod, sandbox.PodResetting)
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("a lost pod's event was reported: %+v", got)
	}
}
