package policy_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/policy"
)

// budget is a policy with a budget of n pods.
func budget(n int) func(*policy.Config) { return func(c *policy.Config) { c.MaxPods = n } }

var (
	poolOnly  = []policy.Kind{policy.KindPool}
	cleanOnly = []policy.Kind{policy.KindClean}
)

// full is the pool when it has no pod to give, none is starting, and live
// pods are alive.
func full(live int) pool { return pool{live: live, noCapacity: true} }

// endedIdle ends a pool task with no key: its pod is idle at once.
func heldAndIdle() []step {
	// A: p1 idle (no key), p2 held pool, p3 held clean.
	return steps(
		runs(1, "p1", poolTask("a1", "A", ""), "p1"),
		runs(2, "p2", poolTask("a2", "A", "issue:2"), "p2"),
		runs(3, "p3", cleanTask("a3", "A", "issue:3"), "p3"),
		[]step{
			{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
			{after: time.Minute, do: ended("a2"), want: []string{"end-lease a2 (done)"}},
			{after: time.Minute, do: ended("a3"), want: []string{"end-lease a3 (done)"}},
		})
}

func TestAPodIsReadyBeforeEachClaim(t *testing.T) {
	run(t, []scenario{
		{
			name:  "with no ready pod, a pod is asked for and nothing is claimed",
			setup: setup(tenant("A", plain)),
			steps: []step{{do: request(pool{live: 0}, "A"), want: []string{"acquire"}}},
		},
		{
			name:  "while a pod is starting nothing is deleted to make room",
			cfg:   budget(1),
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(pool{live: 2, starting: 1, noCapacity: true}, cleanOnly, nil, "A")},
			}),
		},
		{
			name:  "while the budget has room nothing is deleted: creation backs off",
			cfg:   budget(3),
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(full(1), cleanOnly, nil, "A")},
			}),
		},
		{
			name:  "a claim that returns nothing releases its pod, which stays warm",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool res=1 pod=p1"}},
				{do: claimedNothing(1), want: []string{"release res=1 pod=p1"}},
				{do: info("p1"), want: []string{"warm"}},
			},
		},
		{
			name:  "a pool claim is made against an idle pod of its tenant before a warm pod",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(pool{live: 2}, poolOnly, nil, "A"), want: []string{"claim A/pool res=2 pod=p1"}},
			}),
		},
		{
			name:  "a clean claim is made against a warm pod only",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(pool{live: 2}, cleanOnly, nil, "A"), want: []string{"claim A/clean res=2 pod=p2"}},
			}),
		},
		{
			name:  "a warm pod is only given to a tenant its daemon registered for",
			setup: setup(tenant("A", plain), tenant("B", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "B"), want: []string{"acquire"}},
			},
		},
	})
}

func TestBudgetDeletionOrder(t *testing.T) {
	cfg := budget(3)
	run(t, []scenario{
		{
			name:  "an idle pool pod goes first",
			cfg:   cfg,
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A"), warmPod("p3", "A")),
			steps: steps(heldAndIdle(), []step{
				{do: requestKinds(full(3), cleanOnly, nil, "A"), want: []string{"delete p1 (the pod budget is full)"}},
			}),
		},
		{
			name:  "then a held pool pod",
			cfg:   cfg,
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A"), warmPod("p3", "A")),
			steps: steps(heldAndIdle(), []step{
				{do: requestKinds(full(3), cleanOnly, nil, "A"), want: []string{"delete p1 (the pod budget is full)"}},
				{do: requestKinds(full(3), cleanOnly, nil, "A"), want: []string{"delete p2 (the pod budget is full)"}},
				{do: requestKinds(full(3), cleanOnly, nil, "A"), want: []string{"delete p3 (the pod budget is full)"}},
				{do: requestKinds(full(3), cleanOnly, nil, "A")},
			}),
		},
		{
			name:  "a running pod is never deleted",
			cfg:   budget(1),
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: requestKinds(full(1), poolOnly, nil, "A")},
			}),
		},
		{
			name:  "across tenants, the pod of the tenant furthest over its share goes first",
			cfg:   budget(3),
			setup: setup(tenant("A", plain), tenant("B", plain), tenant("C", plain), warmPod("p1", "A", "B", "C"), warmPod("p2", "A", "B", "C"), warmPod("p3", "A", "B", "C")),
			steps: steps(
				runs(1, "p1", poolTask("a1", "A", ""), "p1"),
				runs(2, "p2", poolTask("a2", "A", ""), "p2"),
				runs(3, "p3", poolTask("b1", "B", ""), "p3"),
				[]step{
					{do: ended("b1"), want: []string{"end-lease b1 (done)"}},
					{do: ended("a2"), want: []string{"end-lease a2 (done)"}},
					// A occupies 2, B 1: A's idle pod goes for C, though B's idled first.
					{do: requestKinds(full(3), poolOnly, nil, "C"), want: []string{"delete p2 (the pod budget is full)"}},
				}),
		},
		{
			name:  "the order of kinds comes before the shares",
			cfg:   budget(3),
			setup: setup(tenant("A", plain), tenant("B", plain), tenant("C", plain), warmPod("p1", "A", "B", "C"), warmPod("p2", "A", "B", "C"), warmPod("p3", "A", "B", "C")),
			steps: steps(
				runs(1, "p1", poolTask("a1", "A", ""), "p1"),
				runs(2, "p2", poolTask("a2", "A", "issue:2"), "p2"),
				runs(3, "p3", poolTask("b1", "B", ""), "p3"),
				[]step{
					{do: ended("b1"), want: []string{"end-lease b1 (done)"}},
					{do: ended("a2"), want: []string{"end-lease a2 (done)"}},
					// A's pod is held, B's idle: the idle one goes.
					{do: requestKinds(full(3), poolOnly, nil, "C"), want: []string{"delete p3 (the pod budget is full)"}},
				}),
		},
		{
			name:  "a pod within its tenant's guarantee is never deleted for another tenant",
			cfg:   budget(2),
			setup: setup(tenant("A", policy.TenantSettings{Guarantee: 1}), tenant("B", plain), warmPod("p1", "A", "B"), warmPod("p2", "A", "B")),
			steps: steps(
				runs(1, "p1", poolTask("a1", "A", ""), "p1"),
				runs(2, "p2", poolTask("b1", "B", ""), "p2"),
				[]step{
					{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
					{do: requestKinds(full(2), poolOnly, nil, "B")},
				}),
		},
		{
			name:  "a tenant over its guarantee gives up the pods beyond it",
			cfg:   budget(3),
			setup: setup(tenant("A", policy.TenantSettings{Guarantee: 1}), tenant("B", plain), warmPod("p1", "A", "B"), warmPod("p2", "A", "B"), warmPod("p3", "A", "B")),
			steps: steps(
				runs(1, "p1", poolTask("a1", "A", ""), "p1"),
				runs(2, "p2", poolTask("a2", "A", ""), "p2"),
				runs(3, "p3", poolTask("b1", "B", ""), "p3"),
				[]step{
					{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
					{do: requestKinds(full(3), poolOnly, nil, "B"), want: []string{"delete p1 (the pod budget is full)"}},
				}),
		},
		{
			name:  "a tenant may give up its own pods within its guarantee",
			cfg:   budget(1),
			setup: setup(tenant("A", policy.TenantSettings{Guarantee: 1}), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(full(1), cleanOnly, nil, "A"), want: []string{"delete p1 (the pod budget is full)"}},
			}),
		},
		{
			name:  "without a budget of its own the policy deletes when the pool has nothing and nothing starts",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(full(1), cleanOnly, nil, "A"), want: []string{"delete p1 (the pod budget is full)"}},
			}),
		},
	})
}

func TestGuaranteesAndFairShare(t *testing.T) {
	run(t, []scenario{
		{
			name:  "a guarantee keeps a warm pod from others while its tenant has nothing to run",
			cfg:   budget(2),
			setup: setup(tenant("A", plain), tenant("B", policy.TenantSettings{Guarantee: 1}), warmPod("p1", "A", "B"), warmPod("p2", "A", "B")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: requestKinds(pool{live: 2}, poolOnly, nil, "A")},
				{do: requestKinds(pool{live: 2}, poolOnly, nil, "B"), want: []string{"claim B/pool res=2 pod=p2"}},
			}),
		},
		{
			name:  "the tenant occupying fewer pods is claimed for first, though the other's task is older",
			setup: setup(tenant("A", plain), tenant("B", plain), warmPod("p1", "A", "B"), warmPod("p2", "A", "B")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: pending("A", policy.KindPool, queued("a2", 0, time.Hour))},
				{do: pending("B", policy.KindPool, queued("b1", 0, 0))},
				{do: requestKinds(pool{live: 2}, poolOnly, nil, "A", "B"), want: []string{"claim B/pool res=2 pod=p2"}},
			}),
		},
		{
			name: "weights share the pods beyond the guarantees",
			setup: setup(tenant("A", policy.TenantSettings{Weight: 2}), tenant("B", plain),
				warmPod("p1", "A", "B"), warmPod("p2", "A", "B"), warmPod("p3", "A", "B")),
			steps: []step{
				{do: pending("A", policy.KindPool, queued("a1", 0, 0), queued("a2", 0, 0))},
				{do: pending("B", policy.KindPool, queued("b1", 0, time.Hour), queued("b2", 0, time.Hour))},
				{do: requestKinds(pool{live: 3}, poolOnly, nil, "A", "B"), want: []string{"claim B/pool res=1 pod=p1"}},
				{do: claimed(1, poolTask("b1", "B", "")), want: []string{"place b1 pod=p1"}},
				{do: requestKinds(pool{live: 3}, poolOnly, nil, "A", "B"), want: []string{"claim A/pool res=2 pod=p2"}},
				{do: claimed(2, poolTask("a1", "A", "")), want: []string{"place a1 pod=p2"}},
				{do: requestKinds(pool{live: 3}, poolOnly, nil, "A", "B"), want: []string{"claim A/pool res=3 pod=p3"}},
			},
		},
		{
			name:  "a quota counts running tasks, not held or idle pods",
			setup: setup(tenant("A", policy.TenantSettings{Quota: 1}), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: requestKinds(pool{live: 2}, cleanOnly, nil, "A")},
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: requestKinds(pool{live: 2}, cleanOnly, nil, "A"), want: []string{"claim A/clean res=2 pod=p2"}},
			}),
		},
	})
}

func TestTenantsAndLosses(t *testing.T) {
	run(t, []scenario{
		{
			name:  "a removed tenant's held and idle pods are deleted, and its running pods when their task ends",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A"), warmPod("p3", "A")),
			steps: steps(
				runs(1, "p1", poolTask("a1", "A", ""), "p1"),
				runs(2, "p2", poolTask("a2", "A", "issue:2"), "p2"),
				runs(3, "p3", poolTask("a3", "A", ""), "p3"),
				[]step{
					{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
					{do: ended("a2"), want: []string{"end-lease a2 (done)"}},
					{do: removed("A"), want: []string{"delete p1 (tenant removed)", "delete p2 (tenant removed)"}},
					{do: ended("a3"), want: []string{"end-lease a3 (done)", "delete p3 after a3 (tenant removed)"}},
				}),
		},
		{
			name:  "a lost pod fails its started task",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: lost("p1"), want: []string{"fail a1 (pod lost)", "end-lease a1 (lost)"}},
				{do: info("p1"), want: []string{"unknown"}},
			}),
		},
		{
			name:  "a task whose pod was lost before it started takes the next pod, or fails after the pod wait",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool res=1 pod=p1"}},
				{do: claimed(1, poolTask("a1", "A", "")), want: []string{"place a1 pod=p1"}},
				{do: lost("p1")},
				{do: requestKinds(pool{live: 0}, poolOnly, nil, "A"), want: []string{"acquire"}},
				{do: acquired("p2", "A"), want: []string{"place a1 pod=p2"}},
			},
		},
		{
			name:  "a waiting task fails after the pod wait",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 1}, poolOnly, nil, "A"), want: []string{"claim A/pool res=1 pod=p1"}},
				{do: claimed(1, poolTask("a1", "A", "")), want: []string{"place a1 pod=p1"}},
				{do: lost("p1")},
				{after: 3*time.Minute + time.Second, do: tick(), want: []string{"fail a1 (no pod could take the task in time)"}},
			},
		},
		{
			name:  "a claim whose pod was lost in flight places its task elsewhere",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: []step{
				{do: requestKinds(pool{live: 2}, poolOnly, nil, "A"), want: []string{"claim A/pool res=1 pod=p1"}},
				{do: lost("p1")},
				{do: claimed(1, poolTask("a1", "A", "")), want: []string{"place a1 pod=p2"}},
			},
		},
	})
}
