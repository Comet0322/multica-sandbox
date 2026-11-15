package core_test

import (
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

var bothKinds = []policy.Kind{policy.KindPool, policy.KindClean}

// phase is the policy's phase of a pod the core holds, "" when it holds
// none.
func (r *rig) phase(pod string) policy.Phase {
	pv := r.core.View().Pods[pod]
	if pv.Policy == nil {
		return ""
	}
	return pv.Policy.Phase
}

func TestAPoolAndACleanTaskRunEndToEnd(t *testing.T) {
	r := newRig(t, core.Config{Kinds: bothKinds}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.pool.add("p2", "A")
	r.source.addTask("A", policy.KindPool, "tp", "", "alice", 0)
	r.source.addTask("A", policy.KindClean, "tc", "", "bob", time.Second)

	r.dispatch()
	r.dispatch()
	got := map[string]string{}
	for _, pod := range []string{"p1", "p2"} {
		got[r.next(pod)] = pod
	}
	if got["tp"] == "" || got["tc"] == "" || got["tp"] == got["tc"] {
		t.Fatalf("each task needs a pod of its own: %v", got)
	}
	r.core.TaskEnded(got["tp"], "tp", "pod reported complete")
	r.core.TaskEnded(got["tc"], "tc", "pod reported complete")
	if ph := r.phase(got["tp"]); ph != policy.PhaseIdle {
		t.Fatalf("the pool task's pod: %q, want idle", ph)
	}
	if _, backs := r.pool.snapshot(); !slices.Equal(backs, []string{got["tc"] + " task ended: pod reported complete"}) {
		t.Fatalf("hand-backs %v: the clean task's pod, with no key, goes at once", backs)
	}
}

func TestAHandoffLandsOnTheHeldPod(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: time.Hour}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.pool.add("p2", "A")
	r.source.addTask("A", policy.KindPool, "t1", "issue:1", "alice", 0)
	r.dispatch()
	first := ""
	for _, pod := range []string{"p1", "p2"} {
		if r.next(pod) == "t1" {
			first = pod
		}
	}
	r.core.TaskEnded(first, "t1", "pod reported complete")
	if ph := r.phase(first); ph != policy.PhaseHeld {
		t.Fatalf("t1's pod: %q, want held", ph)
	}

	r.source.addTask("A", policy.KindPool, "t2", "issue:1", "bob", 0)
	r.dispatch()
	if got := r.next(first); got != "t2" {
		t.Fatalf("the held pod picked up %q, want the next task of its issue", got)
	}
	for _, pod := range []string{"p1", "p2"} {
		if pod != first && r.phase(pod) != policy.PhaseWarm {
			t.Fatalf("the pod readied for the claim: %q, want still warm", r.phase(pod))
		}
	}
}

func TestAHeldPodIsDeletedForTheBudgetBeforeAnythingIsClaimed(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: time.Hour}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.addTask("A", policy.KindPool, "t1", "issue:1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.core.TaskEnded("p1", "t1", "pod reported complete")

	r.source.addTask("A", policy.KindPool, "t2", "issue:2", "alice", 0)
	r.dispatch()
	if _, backs := r.pool.snapshot(); !slices.Equal(backs, []string{"p1 the pod budget is full"}) {
		t.Fatalf("hand-backs %v", backs)
	}
	if claims, _, _ := r.source.snapshot(); len(claims) != 1 {
		t.Fatalf("claims %v: nothing is claimed for t2 before a pod is ready", claims)
	}
	r.dispatch()
	if claims, _, _ := r.source.snapshot(); len(claims) != 1 {
		t.Fatalf("claims %v while the new pod starts", claims)
	}

	r.pool.ready("p1")
	r.dispatch()
	if got := r.next("p1"); got != "t2" {
		t.Fatalf("p1 picked up %q, want t2", got)
	}
}

func TestAClaimThatReturnsNothingLeavesThePodWarm(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.dispatch()
	if claims, _, _ := r.source.snapshot(); !slices.Equal(claims, []string{"A"}) {
		t.Fatalf("claims %v, want one probe", claims)
	}
	if ph := r.phase("p1"); ph != policy.PhaseWarm {
		t.Fatalf("p1: %q, want warm", ph)
	}
	if woken, backs := r.pool.snapshot(); len(woken) != 0 || len(backs) != 0 {
		t.Fatalf("woken %v, handed back %v", woken, backs)
	}
	if v := r.core.MayServe("p1", "A"); v != core.Clean {
		t.Fatalf("a warm pod: %v, want clean", v)
	}
}
