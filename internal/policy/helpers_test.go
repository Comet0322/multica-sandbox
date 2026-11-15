package policy_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/policy"
)

// The policy is tested alone, through its methods, by scenarios: a setup and
// steps, each step an event and the decisions it must produce, in order.

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

type step struct {
	// after lets time pass before the step.
	after time.Duration
	do    func(*policy.Policy) []string
	want  []string
}

type scenario struct {
	name  string
	cfg   func(*policy.Config)
	setup func(*policy.Policy)
	steps []step
}

func run(t *testing.T, scenarios []scenario) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			now := t0
			cfg := policy.Config{Now: func() time.Time { return now }, HoldTimeout: 10 * time.Minute, MaxIdle: 24 * time.Hour}
			if sc.cfg != nil {
				sc.cfg(&cfg)
			}
			p, err := policy.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if sc.setup != nil {
				sc.setup(p)
			}
			for i, s := range sc.steps {
				now = now.Add(s.after)
				var got []string
				if s.do != nil {
					got = s.do(p)
				}
				if !slices.Equal(got, s.want) {
					t.Fatalf("step %d: got %q, want %q", i, got, s.want)
				}
			}
		})
	}
}

func describe(ds []policy.Decision) []string {
	var out []string
	for _, d := range ds {
		out = append(out, fmt.Sprint(d))
	}
	return out
}

func setup(fns ...func(*policy.Policy)) func(*policy.Policy) {
	return func(p *policy.Policy) {
		for _, fn := range fns {
			fn(p)
		}
	}
}

func tenant(id string, s policy.TenantSettings) func(*policy.Policy) {
	return func(p *policy.Policy) {
		if err := p.SetTenant(id, s); err != nil {
			panic(err)
		}
	}
}

var plain = policy.TenantSettings{}

// warmPod is a pod acquired before the scenario starts, registered for the
// tenants.
func warmPod(id string, tenants ...string) func(*policy.Policy) {
	return func(p *policy.Policy) { p.PodAcquired(id, tenants...) }
}

func acquired(pod string, tenants ...string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.PodAcquired(pod, tenants...)) }
}

// pool is the state of the pods the policy does not hold, as a claim
// request reports it.
type pool struct {
	live, starting int
	noCapacity     bool
}

func request(pl pool, tenants ...string) func(*policy.Policy) []string {
	return requestKinds(pl, []policy.Kind{policy.KindPool, policy.KindClean}, nil, tenants...)
}

func requestKinds(pl pool, kinds []policy.Kind, exclude []policy.Target, tenants ...string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string {
		return describe(p.RequestClaim(policy.ClaimRequest{
			Tenants: tenants, Kinds: kinds, Exclude: exclude,
			LivePods: pl.live, Starting: pl.starting, NoCapacity: pl.noCapacity,
		}))
	}
}

func claimed(res policy.ClaimID, t policy.Task) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.ClaimReturned(res, &t, false)) }
}

func claimedNothing(res policy.ClaimID) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.ClaimReturned(res, nil, false)) }
}

func claimFailed(res policy.ClaimID) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.ClaimReturned(res, nil, true)) }
}

func started(task string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { p.TaskStarted(task); return nil }
}

func ended(task string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.TaskEnded(task, "done")) }
}

func lost(pod string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.PodLost(pod, "pod lost", "lost")) }
}

func tick() func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.Tick()) }
}

func removed(id string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string { return describe(p.RemoveTenant(id)) }
}

func poolTask(id, tenant, key string) policy.Task {
	return policy.Task{ID: id, Tenant: tenant, Kind: policy.KindPool, Key: key}
}

func cleanTask(id, tenant, key string) policy.Task {
	return policy.Task{ID: id, Tenant: tenant, Kind: policy.KindClean, Key: key}
}

// runs claims t against whatever pod the policy picks and starts it: the
// claim must be for t's tenant and kind, with ID res, made against claimedOn,
// and t must go to pod.
func runs(res policy.ClaimID, claimedOn string, t policy.Task, pod string) []step {
	claim := fmt.Sprintf("claim %s/%s id=%d pod=%s", t.Tenant, t.Kind, res, claimedOn)
	place := []string{"place " + t.ID + " pod=" + pod}
	if pod != claimedOn {
		place = append(place, fmt.Sprintf("closed claim=%d pod=%s", res, claimedOn))
	}
	return []step{
		{do: requestKinds(pool{live: 10}, []policy.Kind{t.Kind}, nil, t.Tenant), want: []string{claim}},
		{do: claimed(res, t), want: place},
		{do: started(t.ID)},
	}
}

func steps(parts ...[]step) []step { return slices.Concat(parts...) }
