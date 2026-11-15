package manager_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// A daemon restarted by the supervisor presents the same pod token, so it
// registers as the same pod. The supervisor says what happened instead.
func TestDaemonExitedEndsTheLeaseAndResetsThePod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")

	if status := pod.event("daemon-exited"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	h.wantEnded("t1", "pod daemon restarted")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("the pod must be reset: %+v", inv)
	}
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("task failure = %q, %v", msg, ok)
	}
}

func TestStartedEventEndsTheLeaseAsAContainerRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	if status := pod.event("started"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	h.wantEnded("t1", "pod container restarted")
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("task failure = %q, %v", msg, ok)
	}
}

func TestEventsLeaveAPodThatHoldsNothingAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	// The sandbox service hands out the pod registered first.
	resetting := h.newPod("pool-0")
	idle := h.newPod("pool-1")
	idle.start()
	leaseAndComplete(t, h, resetting, "t0")

	for _, ev := range []string{"started", "daemon-exited"} {
		if status := idle.event(ev); status != http.StatusOK {
			t.Fatalf("idle %s: status %d", ev, status)
		}
		if status := resetting.event(ev); status != http.StatusOK {
			t.Fatalf("resetting %s: status %d", ev, status)
		}
	}
	inv := h.inventory()
	states := map[string]string{}
	for _, p := range inv.Pods {
		states[p.ID] = p.State
	}
	if states[idle.id] != manager.PodIdle || states[resetting.id] != manager.PodResetting {
		t.Fatalf("pod states changed: %v", states)
	}
	// A pod token nobody issued is refused; the supervisor takes any answer
	// below 500 as delivered and stops resending.
	stranger := &simPod{h: h, name: "stranger", token: "mpt_forged", client: &http.Client{}}
	if status := stranger.event("daemon-exited"); status != http.StatusUnauthorized {
		t.Fatalf("unknown pod: status %d", status)
	}
	// Unknown event names are ignored.
	if status := idle.event("something-new"); status != http.StatusOK {
		t.Fatalf("unknown event: status %d", status)
	}
}

func issueTask(h *harness, id, issue string) {
	h.upstream.mu.Lock()
	defer h.upstream.mu.Unlock()
	h.upstream.queue = append(h.upstream.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": issue, "initiator_email": "alice@example.com",
		"attribution": originated("u-alice"),
	})
}

func TestEventOnAPodEndsItsLeaseAndResetsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour))
	pod := h.newPod("pool-0")
	pod.start()
	issueTask(h, "t1", "issue-1")
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1, got %v", got)
	}

	pod.event("daemon-exited")

	h.wantEnded("t1", "pod daemon restarted")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("the pod must be reset, not kept: %+v", inv)
	}
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("t1 failure = %q, %v", msg, ok)
	}
}

func TestEventOnAHeldPodResetsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour))
	pod := h.newPod("pool-0")
	pod.start()
	issueTask(h, "t1", "issue-1")
	if got := pod.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("got %v", got)
	}
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	if inv := h.inventory(); inv.Leased != 1 {
		t.Fatalf("setup: the pod must be held between tasks: %+v", inv)
	}
	pod.event("started")
	if inv := h.inventory(); inv.Resetting != 1 || inv.Leased != 0 {
		t.Fatalf("a held pod whose container restarted must be reset: %+v", inv)
	}
}

// A task handed to a pod but not yet picked up goes back to the line.
func TestEventPutsTasksAssignedButNotPickedUpBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, withWake(time.Hour))
	first := h.newPod("pool-0")
	first.start()
	sock := first.openWake()
	_ = sock
	h.upstream.enqueue("t1")
	h.dispatch() // claims t1 for the idle pod with a socket and queues it there
	if q := h.upstream.queued(); q != 0 {
		t.Fatalf("setup: t1 was not claimed upstream (%d queued)", q)
	}

	first.event("daemon-exited")

	second := h.newPod("pool-1")
	second.start()
	got := second.claimBatch()
	if !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("the unclaimed task must go to another pod, got %v", got)
	}
}

// The new daemon may register before the event arrives; the event still ends
// what the old one held.
func TestEventStillEndsLeasesAfterTheDaemonRegisteredAsTheSameInstance(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")

	pod.startRaw() // the restarted daemon, same pod token: the same pod
	h.wantHeld("t1")
	pod.event("daemon-exited")
	h.wantEnded("t1", "pod daemon restarted")
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("the pod must be reset: %+v", inv)
	}
}
