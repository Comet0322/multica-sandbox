package lending_test

import (
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

// requestLive is request in a pool of live pods.
func requestLive(live int, pods []string, tenants ...string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.RequestClaim(lending.ClaimRequest{Pods: pods, Tenants: tenants, LivePods: live})
	}
}

// requestLiveNoCapacity is requestLive when the sandbox service has no free
// pod.
func requestLiveNoCapacity(live int, pods []string, tenants ...string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.RequestClaim(lending.ClaimRequest{Pods: pods, Tenants: tenants, LivePods: live, NoCapacity: true})
	}
}

func weighted(w int) lending.TenantSettings { return lending.TenantSettings{Weight: w} }

func guaranteed(g int) lending.TenantSettings { return lending.TenantSettings{Guarantee: g} }

func onTenant(id, ws, issue, initiator string) lending.Task {
	return lending.Task{ID: id, Tenant: ws, Initiator: initiator, Labels: map[string]string{"issue": issue}}
}

func TestFairShare(t *testing.T) {
	// A runs a1 on p1.
	aRuns := []step{
		{do: requestLive(3, pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
		{do: claimed(1, onTenant("a1", "A", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
		{do: started("a1")},
	}
	run(t, []scenario{
		{
			name: "the tenant occupying fewer pods is claimed from first, though the other's task is older",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second))),
			steps: steps(aRuns, []step{
				{do: pending("B", queued("b1", 1, time.Hour))},
				{do: requestLive(3, pods("p2"), "A", "B"), want: []string{"claim B res=2 pod=p2"}},
			}),
		},
		{
			name:  "weights share the pods: with one pod each, the tenant of weight 2 is claimed from next",
			setup: setup(tenant("A", weighted(2)), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"), cleanPod("p3", "A", "B")),
			steps: []step{
				{do: pending("A", queued("a1", 1, time.Hour), queued("a2", 1, time.Hour))},
				{do: pending("B", queued("b1", 1, 0), queued("b2", 1, 0))},
				{do: requestLive(3, pods("p1"), "A", "B"), want: []string{"claim B res=1 pod=p1"}},
				{do: claimed(1, onTenant("b1", "B", "is-1", "bob")), want: []string{"place b1 pod=p1 binding=1 new"}},
				{do: requestLive(3, pods("p2"), "A", "B"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onTenant("a1", "A", "is-2", "alice")), want: []string{"place a1 pod=p2 binding=2 new"}},
				{do: requestLive(3, pods("p3"), "A", "B"), want: []string{"claim A res=3 pod=p3"}},
			},
		},
		{
			name: "a lone busy tenant uses every pod: idle capacity is not kept from it without a guarantee",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second))),
			steps: steps(aRuns, []step{
				{do: requestLive(2, pods("p2"), "A", "B"), want: []string{"claim A res=2 pod=p2"}},
			}),
		},
		{
			name: "a guarantee is kept from others while its tenant has nothing to run",
			setup: setup(tenant("A", unlimited), tenant("B", guaranteed(1)), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second)), pendingSetup("B")),
			steps: steps([]step{
				{do: requestLive(2, pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("a1", "A", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
			}, []step{
				// B's guarantee lets it probe for a task once; A is kept out.
				{do: requestLive(2, pods("p2"), "A", "B"), want: []string{"claim B res=2 pod=p2"}},
				{do: claimedNothing(2), want: []string{"release res=2 pod=p2"}},
				{do: requestLive(2, pods("p2"), "A", "B"), want: []string{}},
				{do: pending("B", queued("b1", 1, time.Hour))},
				{do: requestLive(2, pods("p2"), "A", "B"), want: []string{"claim B res=3 pod=p2"}},
			}),
		},
		{
			name: "no pod is acquired for a tenant the guarantees keep out",
			setup: setup(tenant("A", unlimited), tenant("B", guaranteed(1)), cleanPod("p1", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second))),
			steps: []step{
				{do: requestLive(2, pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("a1", "A", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
				{do: requestLive(2, pods(), "A"), want: []string{}},
			},
		},
		{
			name: "guarantees that add up to more than the live pods are scaled down",
			setup: setup(tenant("A", guaranteed(2)), tenant("B", guaranteed(2)), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second))),
			steps: steps([]step{
				{do: requestLive(2, pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("a1", "A", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
			}, []step{
				// Each keeps one: A is at its share, and B's is kept.
				{do: requestLive(2, pods("p2"), "A"), want: []string{}},
			}),
		},
	})
}

func TestFairEviction(t *testing.T) {
	hold := withHold(time.Hour)
	// A's a1 ran on p1 and p1 is in hold; p2 runs B's b1.
	aHolds := []step{
		{do: requestLive(2, pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
		{do: claimed(1, onTenant("a1", "A", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
		{do: started("a1")},
		{do: requestLive(2, pods("p2"), "B"), want: []string{"claim B res=2 pod=p2"}},
		{do: claimed(2, onTenant("b1", "B", "is-2", "bob")), want: []string{"place b1 pod=p2 binding=2 new"}},
		{do: started("b1")},
		{do: ended("a1", "done"), want: []string{"end-lease a1 (done)"}},
	}
	run(t, []scenario{
		{
			name: "another tenant's hold beyond its guarantee is evicted when no pod is free",
			cfg:  hold,
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0)), pendingSetup("B", queued("b1", 1, 0))),
			steps: steps(aHolds, []step{
				{do: pending("B", queued("b2", 1, 0))},
				{do: requestLiveNoCapacity(2, pods("p1", "p2"), "B"), want: []string{"claim B res=3 pod=p1"}},
			}),
		},
		{
			name: "a tenant the guarantees keep from the free pods ends its own hold instead of waiting for it to run out",
			cfg:  hold,
			setup: setup(tenant("A", guaranteed(1)), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"), cleanPod("p3", "A", "B"),
				pendingSetup("B", queued("b1", 1, 0), queued("b2", 1, time.Second), queued("b3", 1, 2*time.Second))),
			steps: []step{
				{do: requestLive(3, pods("p1"), "B"), want: []string{"claim B res=1 pod=p1"}},
				{do: claimed(1, onTenant("b1", "B", "is-1", "bob")), want: []string{"place b1 pod=p1 binding=1 new"}},
				{do: started("b1")},
				{do: requestLive(3, pods("p2"), "B"), want: []string{"claim B res=2 pod=p2"}},
				{do: claimed(2, onTenant("b2", "B", "is-2", "bob")), want: []string{"place b2 pod=p2 binding=2 new"}},
				{do: started("b2")},
				{do: ended("b1", "done"), want: []string{"end-lease b1 (done)"}},
				// p3 is kept for A; B's hold on p1 is idle and goes to b3 at once.
				{do: requestLive(3, pods("p1", "p2", "p3"), "B"), want: []string{"claim B res=3 pod=p1"}},
			},
		},
		{
			name: "a hold within its tenant's guarantee is never evicted",
			cfg:  hold,
			setup: setup(tenant("A", guaranteed(1)), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0)), pendingSetup("B", queued("b1", 1, 0))),
			steps: steps(aHolds, []step{
				{do: pending("B", queued("b2", 1, 0))},
				{do: requestLiveNoCapacity(2, pods("p1", "p2"), "B"), want: []string{}},
			}),
		},
	})
}

func TestSharesReportTheTargetSplit(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	p, err := lending.New(lending.Config{Now: func() time.Time { return now }, Capacity: lending.Resources{Slots: 1}, Rules: testRules(), Rule: rulePersonIssue})
	if err != nil {
		t.Fatal(err)
	}
	for id, s := range map[string]lending.TenantSettings{"A": {Weight: 2}, "B": {}, "C": {Guarantee: 1}, "D": {}} {
		if err := p.SetTenant(id, s); err != nil {
			t.Fatal(err)
		}
	}
	many := func(prefix string, n int) []lending.Pending {
		var out []lending.Pending
		for i := range n {
			out = append(out, queued(prefix+string(rune('0'+i)), 1, 0))
		}
		return out
	}
	p.PendingChanged("A", many("a", 9))
	p.PendingChanged("B", many("b", 9))
	p.PendingChanged("D", many("d", 1))
	p.RequestClaim(lending.ClaimRequest{LivePods: 10, Tenants: []string{"A", "B", "C", "D"}})
	got := map[string]int{}
	for _, s := range p.Shares() {
		got[s.Tenant] = s.Target
	}
	// C keeps its guarantee; D wants one pod; A and B share the other eight 2:1
	// as far as whole pods go.
	want := map[string]int{"A": 5, "B": 3, "C": 1, "D": 1}
	keys := []string{"A", "B", "C", "D"}
	if !slices.EqualFunc(keys, keys, func(a, _ string) bool { return got[a] == want[a] }) {
		t.Fatalf("targets %v, want %v", got, want)
	}
}
