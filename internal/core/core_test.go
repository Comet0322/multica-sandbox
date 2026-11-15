package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/lending"
)

func TestATaskRunsOnAPodAndThePodGoesBack(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)

	r.dispatch()
	if woken, _ := r.pool.snapshot(); !slices.Equal(woken, []string{"p1"}) {
		t.Fatalf("woken %v, want p1 once its task is placed", woken)
	}
	if r.core.Owns("p1", "t1") {
		t.Fatal("a task is the pod's only once the pod picked it up")
	}
	if got := r.next("p1"); got != "t1" {
		t.Fatalf("p1 picked up %q, want t1", got)
	}
	if got := r.next("p1"); got != "" {
		t.Fatalf("a second pickup returned %q", got)
	}
	if !r.core.Owns("p1", "t1") || r.core.Owns("p2", "t1") {
		t.Fatal("t1 must be p1's and no one else's")
	}
	r.core.TaskEnded("p1", "t1", "pod reported complete")
	if r.core.Owns("p1", "t1") {
		t.Fatal("t1 is over")
	}
	if _, backs := r.pool.snapshot(); !slices.Equal(backs, []string{"p1 task ended: pod reported complete"}) {
		t.Fatalf("hand-backs %v", backs)
	}
	leases := r.core.Leases()
	if len(leases) != 1 || leases[0].EndedAt == nil || leases[0].WorkspaceID != "A" || leases[0].Initiator != "alice@example.com" {
		t.Fatalf("lease record %+v", leases)
	}
}

func TestNothingIsClaimedWithoutAPod(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	if claims, _, _ := r.source.snapshot(); len(claims) != 0 {
		t.Fatalf("claimed %v with no pod in the pool", claims)
	}
}

func TestAnotherPodsTaskIsNotItsOwn(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.pool.add("p2", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	if got := r.next("p2"); got != "" {
		t.Fatalf("p2 picked up %q, placed on p1", got)
	}
	r.next("p1")
	r.core.TaskEnded("p2", "t1", "pod reported complete")
	if !r.core.Owns("p1", "t1") {
		t.Fatal("another pod's report ended p1's task")
	}
}

func TestMayServe(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: time.Hour}, tenant("A", lending.TenantSettings{}), tenant("B", lending.TenantSettings{}))
	r.pool.add("p1", "A", "B")
	if v := r.core.MayServe("p1", "A"); v != core.NotHeld {
		t.Fatalf("a pod the core does not hold: %v", v)
	}
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	if v := r.core.MayServe("p1", "A"); v != core.Allowed {
		t.Fatalf("a pod with a task of A placed on it, for A: %v", v)
	}
	if v := r.core.MayServe("p1", "B"); v != core.Denied {
		t.Fatalf("a pod working for A, for B: %v", v)
	}
	r.next("p1")
	r.core.TaskEnded("p1", "t1", "done")
	if v := r.core.MayServe("p1", "A"); v != core.Denied {
		t.Fatalf("a pod in hold: %v", v)
	}
}

func TestALostPodFailsItsTask(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.pool.lose("p1")
	r.core.PodLost("p1", "silent")
	if _, fails, _ := r.source.snapshot(); !slices.Equal(fails, []string{"A t1 the pod running this task was lost"}) {
		t.Fatalf("fails %v", fails)
	}
	if r.core.Owns("p1", "t1") {
		t.Fatal("the lost pod still owns its task")
	}
}

func TestReconciliationEndsALeaseTheSourceSaysIsOver(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.core.ReconcileOnce(context.Background())
	if !r.core.Owns("p1", "t1") {
		t.Fatal("a running task was ended")
	}
	r.source.mu.Lock()
	r.source.ended["t1"] = "upstream task ended: cancelled"
	r.source.mu.Unlock()
	r.core.ReconcileOnce(context.Background())
	if r.core.Owns("p1", "t1") {
		t.Fatal("the lease outlived the task")
	}
}

func TestALeasePastItsMaximumIsFailed(t *testing.T) {
	r := newRig(t, core.Config{LeaseMax: time.Hour}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.advance(time.Hour + time.Second)
	r.core.Touch()
	if _, fails, _ := r.source.snapshot(); !slices.Equal(fails, []string{"A t1 task exceeded the maximum lease time"}) {
		t.Fatalf("fails %v", fails)
	}
}

func TestARemovedTenantDrainsAndIsClosed(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}), tenant("B", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.core.SetTenants(ctx, []core.Tenant{tenant("B", lending.TenantSettings{})})
	if !slices.Equal(ch.Removed, []string{"A"}) {
		t.Fatalf("changes %+v", ch)
	}
	time.Sleep(20 * time.Millisecond)
	if _, _, closed := r.source.snapshot(); len(closed) != 0 {
		t.Fatalf("closed %v while A still runs a task", closed)
	}
	r.core.TaskEnded("p1", "t1", "pod reported complete")
	eventually(t, "A to be closed", func() bool {
		_, _, closed := r.source.snapshot()
		return slices.Equal(closed, []string{"A"})
	})
}

func TestATenantBackBeforeItsDrainEndedIsNotClosed(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.core.SetTenants(ctx, nil)
	ch := r.core.SetTenants(ctx, []core.Tenant{tenant("A", lending.TenantSettings{})})
	if !slices.Equal(ch.Added, []string{"A"}) {
		t.Fatalf("changes %+v", ch)
	}
	r.core.TaskEnded("p1", "t1", "pod reported complete")
	time.Sleep(30 * time.Millisecond)
	if _, _, closed := r.source.snapshot(); len(closed) != 0 {
		t.Fatalf("closed %v though A came back", closed)
	}
}

func TestAClaimForAnotherTenantIsFailed(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.source.mu.Lock()
	r.source.queues["A"][0].Tenant = "Z"
	r.source.mu.Unlock()
	r.dispatch()
	if _, fails, _ := r.source.snapshot(); len(fails) != 1 || !strings.HasPrefix(fails[0], "A t1 the managed runtime does not serve") {
		t.Fatalf("fails %v", fails)
	}
}

// Fair sharing through the core: the core tells the policy how many pods are
// live, and the policy's choice decides which tenant is claimed from.

func TestTheEmptierTenantIsClaimedFromFirst(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}), tenant("B", lending.TenantSettings{}))
	r.pool.add("p1", "A", "B")
	r.pool.add("p2", "A", "B")
	r.source.add("A", "a1", "alice", 0)
	r.source.add("A", "a2", "alice", time.Second)
	r.source.add("B", "b1", "bob", time.Hour)
	r.dispatch()
	r.dispatch()
	if claims, _, _ := r.source.snapshot(); !slices.Equal(claims, []string{"A", "B"}) {
		t.Fatalf("claims %v: B occupies nothing, so it goes before A's second task", claims)
	}
}

func TestAGuaranteeKeepsAPodForItsTenant(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{}), tenant("B", lending.TenantSettings{Guarantee: 1}))
	r.pool.add("p1", "A", "B")
	r.pool.add("p2", "A", "B")
	r.source.add("A", "a1", "alice", 0)
	r.source.add("A", "a2", "alice", time.Second)
	for range 3 {
		r.dispatch()
	}
	claims, _, _ := r.source.snapshot()
	if n := strings.Count(strings.Join(claims, ","), "A"); n != 1 {
		t.Fatalf("claims %v: A may take one pod, B's guarantee keeps the other", claims)
	}
	r.source.add("B", "b1", "bob", 0)
	r.dispatch()
	if got := r.next("p2"); got != "b1" {
		t.Fatalf("p2 picked up %q, want B's b1", got)
	}
}

func TestTheViewReportsShares(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", lending.TenantSettings{Weight: 2}), tenant("B", lending.TenantSettings{}))
	for _, id := range []string{"p1", "p2", "p3"} {
		r.pool.add(id, "A", "B")
	}
	for i := range 3 {
		r.source.add("A", "a"+string(rune('0'+i)), "alice", 0)
		r.source.add("B", "b"+string(rune('0'+i)), "bob", 0)
	}
	r.dispatch()
	v := r.core.View()
	if v.LivePods != 3 {
		t.Fatalf("live pods %d", v.LivePods)
	}
	got := map[string]int{}
	for _, s := range v.Shares {
		got[s.Tenant] = s.Target
	}
	if got["A"] != 2 || got["B"] != 1 {
		t.Fatalf("targets %v, want A 2 and B 1", got)
	}
}

func TestAuditLogRecordsTheLeaseIdentity(t *testing.T) {
	var buf syncBuffer
	clk := core.NewClock()
	log := core.NewAuditLog(&buf, clk, nil)
	r := newRig(t, core.Config{Audit: log}, tenant("A", lending.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	if s := buf.String(); !strings.Contains(s, `"action":"lease"`) || !strings.Contains(s, `"initiator":"alice@example.com"`) || !strings.Contains(s, `"pod_id":"p1"`) {
		t.Fatalf("audit log %s", s)
	}
}
