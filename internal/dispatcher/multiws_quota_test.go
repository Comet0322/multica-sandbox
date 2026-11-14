package dispatcher_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/dispatcher"
)

// recycleMulti plays the supervisor and restarted daemon for a pod serving
// several workspaces, and returns its runtime IDs.
func (p *simPod) recycleMulti() []string {
	p.h.t.Helper()
	cmd := p.pollControl()
	if cmd == nil || cmd.Command != "reset" {
		p.h.t.Fatalf("want a reset command, got %+v", cmd)
	}
	if code := p.reportControl(cmd.ID, "done"); code != http.StatusOK {
		p.h.t.Fatalf("report done: status %d", code)
	}
	return p.startAll()
}

func TestOnePodReleasedAtATimeSplitsTenAllocationsEvenly(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	// The global queue orders claims oldest first, across workspaces.
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		up.enqueueRanked(a, "a"+string(rune('0'+i)), 0, t0.Add(time.Duration(2*i)*time.Second))
		up.enqueueRanked(b, "b"+string(rune('0'+i)), 0, t0.Add(time.Duration(2*i+1)*time.Second))
	}
	pod := h.newPod("pool-0")
	ids := pod.startAll()

	var fromA, fromB int
	for i := 0; i < 10; i++ {
		got := pod.claimFrom(ids)
		if len(got) != 1 {
			t.Fatalf("allocation %d: want one task, got %v", i, got)
		}
		if strings.HasPrefix(got[0], "a") {
			fromA++
		} else {
			fromB++
		}
		if d := fromA - fromB; d > 1 || d < -1 {
			t.Fatalf("after %d allocations A=%d B=%d differ by more than 1", i+1, fromA, fromB)
		}
		if status := pod.report(got[0], "complete"); status != http.StatusOK {
			t.Fatalf("complete: status %d", status)
		}
		ids = pod.recycleMulti()
	}
	if fromA != 5 || fromB != 5 {
		t.Fatalf("want 5 each, got A=%d B=%d", fromA, fromB)
	}
}

func TestEmptyWorkspaceInRotationDoesNotHoldThePodBack(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	// Only B has work; the rotation must not make the pod wait for A.
	up.enqueue(b, "b1")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("an empty workspace must not hold the pod back, got %v", got)
	}
}

func TestIdlePodServesUnderQuotaWorkspaceInOnePollWhileOtherIsCapped(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(func(c *dispatcher.WorkspaceConfig) { c.Quota = 1 }, nil))
	p1, p2 := h.newPod("pool-0"), h.newPod("pool-1")
	ids1, ids2 := p1.startAll(), p2.startAll()
	up.enqueue(a, "ta1")
	up.enqueue(a, "ta2")
	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}

	// A is at its cap with a backlog. B has nothing yet: the idle pod gets
	// nothing, and A is not polled for it.
	aClaims := up.claimCount(a)
	if got := p2.claimFrom(ids2); len(got) != 0 {
		t.Fatalf("capped workspace must not get a second pod, got %v", got)
	}
	// B's backlog appears: the very next poll of the idle pod takes it.
	up.enqueue(b, "tb1")
	if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("idle pod must serve B on its next poll, got %v", got)
	}
	if n := up.claimCount(a); n != aClaims {
		t.Fatalf("capped workspace was polled: %d -> %d", aClaims, n)
	}

	// A drops below its cap: its backlog flows again on the next poll.
	if status := p1.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	ids1 = p1.recycleMulti()
	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta2"}) {
		t.Fatalf("A is under its cap again, want [ta2], got %v", got)
	}
}
