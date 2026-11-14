package lending_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

// keyOf places t alone on a fresh pod under rule and returns the affinity key
// of the binding it opened.
func keyOf(t *testing.T, rules lending.Registry, rule lending.AffinityRule, tk lending.Task) string {
	t.Helper()
	if rules == nil {
		rules = testRules()
	}
	p, err := lending.New(lending.Config{Capacity: lending.Resources{Slots: 1}, Rule: rule, Rules: rules})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.SetTenant(tk.Tenant, unlimited)
	p.PodAcquired("p1", tk.Tenant)
	p.RequestClaim(lending.ClaimRequest{Pods: pods("p1"), Tenants: []string{tk.Tenant}})
	p.ClaimReturned(1, &tk, false)
	b, ok := p.Binding("p1")
	if !ok {
		t.Fatalf("no binding on p1")
	}
	return b.Key
}

func TestRulesProduceTheDocumentedKey(t *testing.T) {
	full := lending.Task{ID: "t1", Tenant: "A", Initiator: "u-alice", Labels: map[string]string{"agent": "ag-1", "issue": "is-1", "chat": "ch-1"}}
	chat := lending.Task{ID: "t2", Tenant: "A", Initiator: "u-alice", Labels: map[string]string{"agent": "ag-1", "chat": "ch-1"}}
	bare := lending.Task{ID: "t3", Tenant: "A", Initiator: "u-alice", Labels: map[string]string{"agent": "ag-1"}}
	anon := lending.Task{ID: "t4", Tenant: "A", Labels: map[string]string{"agent": "ag-1", "issue": "is-1"}}
	for _, c := range []struct {
		rule lending.AffinityRule
		task lending.Task
		want string
	}{
		{lending.RuleTask, full, "task:t1"},
		{rulePersonIssue, full, "person-issue:issue:is-1/by:u-alice/in:A"},
		{rulePersonIssue, chat, "person-issue:chat:ch-1/by:u-alice/in:A"},
		{rulePersonIssue, bare, "task:t3"},
		{rulePersonAgent, full, "person-agent:ag-1/by:u-alice/in:A"},
		{rulePerson, full, "person:all/by:u-alice/in:A"},
		{rulePerson, anon, "task:t4"},
		{rulePersonIssue, anon, "task:t4"},
		{rulePersonAgent, anon, "task:t4"},
	} {
		if got := keyOf(t, nil, c.rule, c.task); got != c.want {
			t.Errorf("%s for %s: key %q, want %q", c.rule, c.task.ID, got, c.want)
		}
	}
}

func TestANewRuleIsOneRegisteredFunction(t *testing.T) {
	rules := testRules()
	rules["by-agent-and-issue"] = func(t lending.Task) string { return t.Labels["agent"] + "+" + t.Labels["issue"] }
	tk := lending.Task{ID: "t1", Tenant: "A", Initiator: "u-alice", Labels: map[string]string{"agent": "ag-1", "issue": "is-1"}}
	if got, want := keyOf(t, rules, "by-agent-and-issue", tk), "by-agent-and-issue:ag-1+is-1/by:u-alice/in:A"; got != want {
		t.Fatalf("key %q, want %q", got, want)
	}
}

func TestTheDefaultRuleGivesEveryTaskItsOwnBinding(t *testing.T) {
	tk := lending.Task{ID: "t1", Tenant: "A", Initiator: "u-alice", Labels: map[string]string{"issue": "is-1"}}
	if got, want := keyOf(t, lending.Registry{}, "", tk), "task:t1"; got != want {
		t.Fatalf("key %q, want %q", got, want)
	}
}

// sameUnit is a task of the given person on issue is-1, chat ch-1 and agent
// ag-1, so every built-in rule but task would let two of them share.
func sameUnit(id, initiator string) lending.Task {
	return lending.Task{ID: id, Tenant: "A", Initiator: initiator, Labels: map[string]string{"agent": "ag-1", "issue": "is-1", "chat": "ch-1"}}
}

func underRule(rule lending.AffinityRule) func(c *lending.Config) {
	return func(c *lending.Config) { c.Rule, c.Capacity = rule, lending.Resources{Slots: 4} }
}

func TestSharingUnderEveryBuiltinRule(t *testing.T) {
	three := setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"), cleanPod("p3", "A"))
	var scenarios []scenario
	for _, rule := range []lending.AffinityRule{rulePersonIssue, rulePersonAgent, rulePerson} {
		scenarios = append(scenarios,
			scenario{
				name: string(rule) + ": tasks of two people never share a binding; one person's do", cfg: underRule(rule), setup: three,
				steps: []step{
					{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
					{do: claimed(1, sameUnit("t1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
					{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
					{do: claimed(2, sameUnit("t2", "bob")), want: []string{"place t2 pod=p2 binding=2 new"}},
					{do: request(pods("p3"), "A"), want: []string{"claim A res=3 pod=p3"}},
					{do: claimed(3, sameUnit("t3", "alice")), want: []string{"place t3 pod=p1 binding=1", "release res=3 pod=p3"}},
				},
			},
			scenario{
				name: string(rule) + ": tasks without an initiator each run in a binding of their own", cfg: underRule(rule), setup: three,
				steps: []step{
					{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
					{do: claimed(1, sameUnit("t1", "")), want: []string{"place t1 pod=p1 binding=1 new"}},
					{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
					{do: claimed(2, sameUnit("t2", "")), want: []string{"place t2 pod=p2 binding=2 new"}},
				},
			},
		)
	}
	scenarios = append(scenarios, scenario{
		name: "task: one person's tasks never share a binding", cfg: underRule(lending.RuleTask), setup: three,
		steps: []step{
			{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
			{do: claimed(1, sameUnit("t1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
			{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			{do: claimed(2, sameUnit("t2", "alice")), want: []string{"place t2 pod=p2 binding=2 new"}},
		},
	}, scenario{
		name: "the same person in two tenants does not share a binding",
		cfg:  underRule(rulePerson),
		setup: setup(tenant("A", unlimited), tenant("B", unlimited),
			cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B")),
		steps: []step{
			{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
			{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
			{do: request(pods("p2"), "B"), want: []string{"claim B res=2 pod=p2"}},
			{do: claimed(2, task("t2", "B", "alice")), want: []string{"place t2 pod=p2 binding=2 new"}},
		},
	})
	run(t, scenarios)
}

func withHold(d time.Duration) func(c *lending.Config) {
	return func(c *lending.Config) {
		c.Rule, c.HoldTimeout, c.Capacity = rulePersonIssue, d, lending.Resources{Slots: 2}
	}
}

func onIssue(id, issue, initiator string) lending.Task {
	return lending.Task{ID: id, Tenant: "A", Initiator: initiator, Labels: map[string]string{"issue": issue}}
}

func TestHold(t *testing.T) {
	two := setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"))
	firstTask := []step{
		{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
		{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
		{do: started("t1")},
	}
	run(t, []scenario{
		{
			name: "a binding left without a task holds its pod, and the pod is reset when the hold runs out",
			cfg:  withHold(10 * time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: request(pods("p1"), "A"), want: []string{}},
				{after: 10*time.Minute - time.Second, do: tick(), want: []string{}},
				{after: time.Second, do: tick(), want: []string{"reset p1 (hold expired)"}},
				{do: tick(), want: []string{}},
				{do: request(pods("p1", "p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			}),
		},
		{
			name: "a matching task during hold takes the pod, and the countdown restarts when it ends",
			cfg:  withHold(10 * time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{after: 8 * time.Minute, do: request(pods("p1", "p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: started("t2")},
				{after: 5 * time.Minute, do: tick(), want: []string{}},
				{do: ended("t2", "done"), want: []string{"end-lease t2 (done)"}},
				{after: 10*time.Minute - time.Second, do: tick(), want: []string{}},
				{after: time.Second, do: tick(), want: []string{"reset p1 (hold expired)"}},
			}),
		},
		{
			name: "a task of another key does not take a pod in hold",
			cfg:  withHold(10 * time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: request(pods("p1", "p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "bob")), want: []string{"place t2 pod=p2 binding=2 new"}},
			}),
		},
		{
			name: "a hold timeout of 0 resets the pod as soon as its task ends",
			cfg:  withHold(0), setup: two,
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p1 after t1 (task ended: done)"}},
			}),
		},
		{
			name: "a binding whose key no other task can have is reset at once, whatever the hold timeout",
			cfg:  withHold(10 * time.Minute), setup: two,
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onIssue("t1", "is-1", "")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p1 after t1 (task ended: done)"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "", "alice")), want: []string{"place t2 pod=p2 binding=2 new"}},
				{do: ended("t2", "done"), want: []string{"end-lease t2 (done)", "reset p2 after t2 (task ended: done)"}},
			},
		},
		{
			name: "a tenant's own hold timeout applies to its bindings",
			cfg:  withHold(10 * time.Minute),
			setup: setup(tenant("A", lending.TenantSettings{HoldTimeout: durationPtr(time.Minute)}),
				cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{after: time.Minute, do: tick(), want: []string{"reset p1 (hold expired)"}},
			}),
		},
		{
			name: "a tenant hold timeout of 0 overrides the pool's",
			cfg:  withHold(10 * time.Minute),
			setup: setup(tenant("A", lending.TenantSettings{HoldTimeout: durationPtr(0)}),
				cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p1 after t1 (task ended: done)"}},
			}),
		},
		{
			name: "a lost pod in hold is forgotten without failing anything",
			cfg:  withHold(10 * time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: lost("p1"), want: []string{}},
				{after: 11 * time.Minute, do: tick(), want: []string{}},
			}),
		},
		{
			name:  "the most recently used binding of a key with room is chosen, in hold or running",
			cfg:   withHold(10 * time.Minute),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"), cleanPod("p3", "A")),
			steps: steps(firstTask, []step{
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{after: time.Second, do: request(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, onIssue("t3", "is-1", "alice")), want: []string{"place t3 pod=p2 binding=2 new"}},
				{do: ended("t3", "done"), want: []string{"end-lease t3 (done)"}},
				{do: request(pods("p3"), "A"), want: []string{"claim A res=4 pod=p3"}},
				{do: claimed(4, onIssue("t4", "is-1", "alice")), want: []string{"place t4 pod=p2 binding=2", "release res=4 pod=p3"}},
			}),
		},
	})
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func TestBindingQueryShowsKeyAndHold(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	p, err := lending.New(lending.Config{
		Now: func() time.Time { return now }, Capacity: lending.Resources{Slots: 2},
		Rules: testRules(), Rule: rulePersonIssue, HoldTimeout: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.SetTenant("A", unlimited)
	p.PodAcquired("p1", "A")
	if _, ok := p.Binding("p1"); ok {
		t.Fatal("an idle pod has a binding")
	}
	p.RequestClaim(lending.ClaimRequest{Pods: pods("p1"), Tenants: []string{"A"}})
	p.ClaimReturned(1, &lending.Task{ID: "t1", Tenant: "A", Initiator: "alice", Labels: map[string]string{"issue": "is-1"}}, false)
	b, _ := p.Binding("p1")
	want := lending.BindingInfo{ID: 1, Key: "person-issue:issue:is-1/by:alice/in:A", Tenant: "A", Used: lending.Resources{Slots: 1}, Tasks: 1}
	if b != want {
		t.Fatalf("running binding %+v, want %+v", b, want)
	}
	now = now.Add(time.Minute)
	p.TaskEnded("t1", "done")
	b, _ = p.Binding("p1")
	want = lending.BindingInfo{ID: 1, Key: want.Key, Tenant: "A", Hold: true, HoldUntil: now.Add(10 * time.Minute)}
	if b != want {
		t.Fatalf("held binding %+v, want %+v", b, want)
	}
}
