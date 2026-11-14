package lending_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

// A scenario is a sequence of events fed to one policy, each with the
// decisions it must produce, in order. Time only moves when a step says so.

type step struct {
	// after advances the fake clock before the event.
	after time.Duration
	do    func(p *lending.Policy) []lending.Decision
	want  []string
}

type scenario struct {
	name  string
	cfg   func(c *lending.Config)
	setup func(p *lending.Policy)
	steps []step
}

func run(t *testing.T, scenarios []scenario) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
			cfg := lending.Config{Now: func() time.Time { return now }, Capacity: lending.Resources{Slots: 1}, Rules: testRules(), Rule: rulePersonIssue}
			if sc.cfg != nil {
				sc.cfg(&cfg)
			}
			p, err := lending.New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if sc.setup != nil {
				sc.setup(p)
			}
			for i, s := range sc.steps {
				now = now.Add(s.after)
				got := describe(s.do(p))
				if !slices.Equal(got, s.want) {
					t.Fatalf("step %d:\n got  %q\n want %q", i, got, s.want)
				}
			}
		})
	}
}

func describe(ds []lending.Decision) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, fmt.Sprint(d))
	}
	return out
}

// Event helpers, so scenario tables read as event lists.

func tenant(id string, s lending.TenantSettings) func(*lending.Policy) {
	return func(p *lending.Policy) {
		if err := p.SetTenant(id, s); err != nil {
			panic(err)
		}
	}
}

func setup(fns ...func(*lending.Policy)) func(*lending.Policy) {
	return func(p *lending.Policy) {
		for _, fn := range fns {
			fn(p)
		}
	}
}

// cleanPod acquires a pod whose daemon serves tenants.
func cleanPod(id string, tenants ...string) func(*lending.Policy) {
	return func(p *lending.Policy) { p.PodAcquired(id, tenants...) }
}

func acquired(pod string, tenants ...string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.PodAcquired(pod, tenants...) }
}

func registered(pod, ws string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.PodRegistered(pod, ws) }
}

// requestNoCapacity is request when the sandbox service has no free pod.
func requestNoCapacity(pods []string, tenants ...string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.RequestClaim(lending.ClaimRequest{Pods: pods, Tenants: tenants, NoCapacity: true})
	}
}

func request(pods []string, tenants ...string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.RequestClaim(lending.ClaimRequest{Pods: pods, Tenants: tenants})
	}
}

func requestExcluding(pods []string, exclude []string, tenants ...string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.RequestClaim(lending.ClaimRequest{Pods: pods, Tenants: tenants, Exclude: exclude})
	}
}

func pods(ids ...string) []string { return ids }

func claimed(res lending.ReservationID, t lending.Task) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.ClaimReturned(res, &t, false) }
}

func claimedNothing(res lending.ReservationID) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.ClaimReturned(res, nil, false) }
}

func claimFailed(res lending.ReservationID) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.ClaimReturned(res, nil, true) }
}

func started(task string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { p.TaskStarted(task); return nil }
}

func ended(task, reason string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.TaskEnded(task, reason) }
}

func lost(pod string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.PodLost(pod, "the pod running this task was lost", "pod lost")
	}
}

func interrupted(pod string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		return p.PodInterrupted(pod, "the pod's daemon restarted", "pod daemon restarted")
	}
}

func tick() func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.Tick() }
}

func task(id, ws, initiator string) lending.Task {
	return lending.Task{ID: id, Tenant: ws, Initiator: initiator}
}

var unlimited = lending.TenantSettings{}

func TestLeaseResetIdleCycle(t *testing.T) {
	run(t, []scenario{
		{
			name:  "a claimed task opens a binding on the reserved pod and its end resets the pod",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{do: ended("t1", "pod reported complete"), want: []string{
					"end-lease t1 (pod reported complete)",
					"reset p1 after t1 (task ended: pod reported complete)",
				}},
				{do: request(pods("p1"), "A"), want: []string{"acquire"}},
				{do: acquired("p1", "A"), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=2 pod=p1"}},
			},
		},
		{
			name:  "with no pod held a claim asks for one to be acquired, and with no capacity nothing is claimed",
			setup: setup(tenant("A", unlimited)),
			steps: []step{
				{do: request(pods(), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods(), "A"), want: []string{}},
				{do: acquired("p1", "A"), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
			},
		},
		{
			name:  "an acquired pod that ran no task is kept for the next claim",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimedNothing(1), want: []string{"release res=1 pod=p1"}},
				{do: tick(), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=2 pod=p1"}},
			},
		},
	})
}

func TestReservation(t *testing.T) {
	run(t, []scenario{
		{
			name:  "no claim is made when no pod can take a new binding",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p1"), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1"), "A"), want: []string{}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: requestNoCapacity(pods("p1"), "A"), want: []string{}},
			},
		},
		{
			name:  "an empty claim releases its reservation",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimedNothing(1), want: []string{"release res=1 pod=p1"}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=2 pod=p1"}},
			},
		},
		{
			name:  "a pod without the tenant, an unknown pod or an unserved tenant gets no reservation",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "B")),
			steps: []step{
				{do: request(pods("p1", "p9"), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1", "p9"), "A"), want: []string{}},
				{do: request(pods("p1"), "B"), want: []string{}},
			},
		},
		{
			name:  "a held pod is lent to a tenant once its daemon registers for it",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "B")),
			steps: []step{
				{do: registered("p1", "A"), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
			},
		},
		{
			name:  "a daemon registering on a pod not held changes nothing",
			setup: setup(tenant("A", unlimited)),
			steps: []step{
				{do: registered("p9", "A"), want: []string{}},
				{do: requestNoCapacity(pods("p9"), "A"), want: []string{}},
			},
		},
		{
			name:  "the first pod of the request that can take a binding is reserved",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p1", "p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			},
		},
		{
			name:  "a failed claim releases its reservation and backs the tenant off",
			cfg:   func(c *lending.Config) { c.Backoff = 10 * time.Second },
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B")),
			steps: []step{
				{do: request(pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimFailed(1), want: []string{"release res=1 pod=p1"}},
				{do: request(pods("p1"), "A", "B"), want: []string{"claim B res=2 pod=p1"}},
				{do: claimedNothing(2), want: []string{"release res=2 pod=p1"}},
				{do: request(pods("p1"), "A"), want: []string{}},
				{after: 11 * time.Second, do: request(pods("p1"), "A"), want: []string{"claim A res=3 pod=p1"}},
			},
		},
		{
			name:  "a task claimed for a pod lost meanwhile goes to a free pod",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: lost("p1"), want: []string{}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p2 binding=1 new"}},
			},
		},
		{
			name:  "a task claimed for a pod lost meanwhile waits for the next acquired pod when no pod is free",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: lost("p1"), want: []string{}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{}},
				{do: claimedNothing(1), want: []string{}},
				{do: request(pods(), "A"), want: []string{"acquire"}},
				{do: acquired("p2", "A"), want: []string{"place t1 pod=p2 binding=1 new"}},
			},
		},
	})
}

// sharedRules adds a test rule that lets every task of one person share a
// pod, to exercise capacity before the shared built-in rules exist.
func sharedRules() lending.Registry {
	r := testRules()
	r["shared"] = func(lending.Task) string { return "all" }
	return r
}

func shared(slots int) func(c *lending.Config) {
	return func(c *lending.Config) {
		c.Rules, c.Rule, c.Capacity = sharedRules(), "shared", lending.Resources{Slots: slots}
	}
}

func TestCapacityAndAffinity(t *testing.T) {
	run(t, []scenario{
		{
			name:  "the task rule gives every task its own binding whatever the capacity",
			cfg:   func(c *lending.Config) { c.Capacity = lending.Resources{Slots: 4} },
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "alice")), want: []string{"place t2 pod=p2 binding=2 new"}},
			},
		},
		{
			name:  "a pod with capacity for 2 slots runs at most 2 tasks; the third opens a binding on the reserved pod",
			cfg:   shared(2),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, task("t3", "A", "alice")), want: []string{"place t3 pod=p2 binding=2 new"}},
			},
		},
		{
			name:  "with several bindings of one key the most recently used one with room is chosen",
			cfg:   shared(2),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"), cleanPod("p3", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{after: time.Second, do: request(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, task("t3", "A", "alice")), want: []string{"place t3 pod=p2 binding=2 new"}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: request(pods("p3"), "A"), want: []string{"claim A res=4 pod=p3"}},
				{do: claimed(4, task("t4", "A", "alice")), want: []string{"place t4 pod=p2 binding=2", "release res=4 pod=p3"}},
				{do: request(pods("p3"), "A"), want: []string{"claim A res=5 pod=p3"}},
				{do: claimed(5, task("t5", "A", "alice")), want: []string{"place t5 pod=p1 binding=1", "release res=5 pod=p3"}},
			},
		},
		{
			name:  "tasks of different people never share a binding",
			cfg:   shared(4),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "bob")), want: []string{"place t2 pod=p2 binding=2 new"}},
			},
		},
		{
			name:  "a task without an initiator runs in a binding of its own",
			cfg:   shared(4),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "")), want: []string{"place t2 pod=p2 binding=2 new"}},
			},
		},
		{
			name:  "a shared binding resets only after its last task ends",
			cfg:   shared(2),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: ended("t2", "done"), want: []string{"end-lease t2 (done)", "reset p1 after t2 (task ended: done)"}},
			},
		},
	})
}

func TestQuotaAndRotation(t *testing.T) {
	run(t, []scenario{
		{
			name: "tenants take turns",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited),
				cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"), cleanPod("p3", "A", "B")),
			steps: []step{
				{do: request(pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p2"), "A", "B"), want: []string{"claim B res=2 pod=p2"}},
				{do: request(pods("p3"), "A", "B"), want: []string{"claim A res=3 pod=p3"}},
			},
		},
		{
			name: "a tenant at its quota is skipped; its claims in flight count",
			setup: setup(tenant("A", lending.TenantSettings{Quota: 1}), tenant("B", unlimited),
				cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"), cleanPod("p3", "A", "B")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A", "B"), want: []string{"claim B res=2 pod=p2"}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p1 after t1 (task ended: done)"}},
				{do: request(pods("p3"), "A", "B"), want: []string{"claim A res=3 pod=p3"}},
			},
		},
		{
			name:  "excluded tenants are not claimed from in this round",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B")),
			steps: []step{
				{do: requestExcluding(pods("p1"), []string{"A"}, "A", "B"), want: []string{"claim B res=1 pod=p1"}},
				{do: claimedNothing(1), want: []string{"release res=1 pod=p1"}},
				{do: requestExcluding(pods("p1"), []string{"A", "B"}, "A", "B"), want: []string{}},
			},
		},
	})
}

// leased claims a task for pod and places it there.
func leased(pod, id string, res lending.ReservationID) []step {
	return []step{
		{do: request(pods(pod), "A"), want: []string{fmt.Sprintf("claim A res=%d pod=%s", res, pod)}},
		{do: claimed(res, task(id, "A", "alice")), want: []string{fmt.Sprintf("place %s pod=%s binding=%d new", id, pod, res)}},
	}
}

func steps(parts ...[]step) []step {
	var out []step
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestForcedEnds(t *testing.T) {
	run(t, []scenario{
		{
			name:  "a lost pod's started task is failed and its lease ended, and the pod is forgotten",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: steps(leased("p1", "t1", 1), []step{
				{do: started("t1")},
				{do: lost("p1"), want: []string{"fail t1 (the pod running this task was lost)", "end-lease t1 (pod lost)"}},
				{do: lost("p1"), want: []string{}},
				{do: registered("p1", "A"), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"acquire"}},
			}),
		},
		{
			name:  "a task not yet picked up from a lost pod goes to the next free pod, and nothing is claimed meanwhile",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: steps(leased("p1", "t1", 1), []step{
				{do: lost("p1"), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1"), "A"), want: []string{}},
				{do: acquired("p2", "A"), want: []string{"place t1 pod=p2 binding=2 new"}},
				{do: started("t1")},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p2 after t1 (task ended: done)"}},
			}),
		},
		{
			name:  "a displaced task that finds no pod within the placement wait is failed",
			cfg:   func(c *lending.Config) { c.PlacementWait = 30 * time.Second },
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: steps(leased("p1", "t1", 1), leased("p2", "t2", 2), []step{
				{do: lost("p1"), want: []string{}},
				{after: 30 * time.Second, do: tick(), want: []string{}},
				{after: time.Second, do: tick(), want: []string{"fail t1 (no sandbox pod could take the task in time)"}},
				{do: tick(), want: []string{}},
			}),
		},
		{
			name:  "an interrupted pod's started tasks are failed, its unstarted ones placed elsewhere, and the pod forgotten without a hand-back",
			cfg:   shared(2),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{do: acquired("p2", "A"), want: []string{}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("t2", "A", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: interrupted("p1"), want: []string{
					"fail t1 (the pod's daemon restarted)", "end-lease t1 (pod daemon restarted)", "place t2 pod=p2 binding=2 new",
				}},
				{do: interrupted("p1"), want: []string{}},
			},
		},
		{
			name:  "an interrupted pod without a binding is forgotten",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A")),
			steps: []step{
				{do: interrupted("p1"), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"acquire"}},
			},
		},
		{
			name:  "a task the policy does not know still has its lease ended",
			setup: setup(tenant("A", unlimited)),
			steps: []step{
				{do: ended("t9", "pod reported complete"), want: []string{"end-lease t9 (pod reported complete)"}},
			},
		},
	})
}

func TestTenantBusy(t *testing.T) {
	p, err := lending.New(lending.Config{Capacity: lending.Resources{Slots: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.SetTenant("A", unlimited)
	p.PodAcquired("p1", "A")
	if p.Busy("A") {
		t.Fatal("an idle tenant is busy")
	}
	p.RequestClaim(lending.ClaimRequest{Pods: pods("p1"), Tenants: []string{"A"}})
	if !p.Busy("A") {
		t.Fatal("a claim in flight must keep the tenant busy")
	}
	p.ClaimReturned(1, &lending.Task{ID: "t1", Tenant: "A"}, false)
	p.RemoveTenant("A")
	if !p.Busy("A") {
		t.Fatal("a removed tenant's running task must keep it busy")
	}
	p.TaskEnded("t1", "done")
	if p.Busy("A") {
		t.Fatal("busy after its last task ended")
	}
}

func TestConfigIsValidated(t *testing.T) {
	for name, cfg := range map[string]lending.Config{
		"no capacity":      {},
		"unknown rule":     {Capacity: lending.Resources{Slots: 1}, Rule: "pod"},
		"negative backoff": {Capacity: lending.Resources{Slots: 1}, Backoff: -time.Second},
	} {
		if _, err := lending.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, _ := lending.New(lending.Config{Capacity: lending.Resources{Slots: 1}})
	if err := p.SetTenant("A", lending.TenantSettings{Rule: "pod"}); err == nil {
		t.Error("unknown tenant rule accepted")
	}
	if err := p.SetTenant("A", lending.TenantSettings{Quota: -1}); err == nil {
		t.Error("negative quota accepted")
	}
}
