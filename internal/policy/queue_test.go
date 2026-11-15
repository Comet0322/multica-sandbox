package policy_test

import (
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/policy"
)

func queued(id string, priority int, age time.Duration) policy.Pending {
	return policy.Pending{ID: id, Priority: priority, CreatedAt: t0.Add(-age), Status: policy.StatusQueued}
}

func pending(tenant string, kind policy.Kind, tasks ...policy.Pending) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { p.PendingChanged(tenant, kind, tasks); return nil }
}

func TestClaimOrder(t *testing.T) {
	both := []policy.Kind{policy.KindPool, policy.KindClean}
	run(t, []scenario{
		{
			name:  "within a tenant, the kind whose next task waited longest is claimed for first",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: pending("A", policy.KindPool, queued("a1", 0, time.Minute))},
				{do: pending("A", policy.KindClean, queued("a2", 0, time.Hour))},
				{do: requestKinds(pool{live: 1}, both, nil, "A"), want: []string{"claim A/clean id=1 pod=p1"}},
			},
		},
		{
			name:  "a tenant with an empty list is probed once per list, then left alone",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: pending("A", policy.KindPool)},
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool id=1 pod=p1"}},
				{do: claimedNothing(1), want: []string{"closed claim=1 pod=p1"}},
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A")},
				{do: pending("A", policy.KindPool)},
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool id=2 pod=p1"}},
			},
		},
		{
			name:  "a target tried this round is skipped",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, both, []policy.Target{{Tenant: "A", Kind: policy.KindPool}}, "A"), want: []string{"claim A/clean id=1 pod=p1"}},
			},
		},
		{
			name:  "a tenant whose claim failed backs off",
			cfg:   func(c *policy.Config) { c.Backoff = 10 * time.Second },
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool id=1 pod=p1"}},
				{do: claimFailed(1), want: []string{"closed claim=1 pod=p1"}},
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A")},
				{after: 10 * time.Second, do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool id=2 pod=p1"}},
			},
		},
		{
			name:  "a pod a claim in flight was made against takes no second claim",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool id=1 pod=p1"}},
				{do: requestKinds(pool{live: 1}, cleanOnly, nil, "A"), want: []string{"acquire"}},
			},
		},
	})
}

func TestQueuedListsATenantsTasksInClaimOrder(t *testing.T) {
	p, _ := policy.New(policy.Config{Now: func() time.Time { return t0 }})
	p.PendingChanged("A", policy.KindPool, []policy.Pending{queued("a1", 0, time.Minute), queued("a2", 1, 0)})
	p.PendingChanged("A", policy.KindClean, []policy.Pending{queued("a3", 0, time.Hour), {ID: "a4", Status: "dispatched"}})
	var got []string
	for _, q := range p.Queued("A") {
		got = append(got, q.ID)
	}
	if want := []string{"a2", "a3", "a1"}; !slices.Equal(got, want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
}
