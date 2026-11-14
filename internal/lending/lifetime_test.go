package lending_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

func withLifetime(hold, life, grace time.Duration) func(c *lending.Config) {
	return func(c *lending.Config) {
		c.Rule, c.Capacity = rulePersonIssue, lending.Resources{Slots: 2}
		c.HoldTimeout, c.MaxLifetime, c.DrainGrace = hold, life, grace
	}
}

func TestMaximumLifetime(t *testing.T) {
	two := setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"))
	firstTask := []step{
		{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
		{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
		{do: started("t1")},
	}
	run(t, []scenario{
		{
			name: "a binding past its maximum lifetime takes no new task and is reset when its last task ends",
			cfg:  withLifetime(10*time.Minute, time.Hour, 10*time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{after: time.Hour - time.Second, do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: started("t2")},
				{do: ended("t2", "done"), want: []string{"end-lease t2 (done)"}},
				{after: time.Second, do: request(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, onIssue("t3", "is-1", "alice")), want: []string{"place t3 pod=p2 binding=2 new"}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p1 after t1 (maximum lifetime reached)"}},
			}),
		},
		{
			name: "a binding in hold is reset when it reaches its maximum lifetime",
			cfg:  withLifetime(30*time.Minute, time.Hour, 10*time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{after: 50 * time.Minute, do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{after: 10*time.Minute - time.Second, do: tick(), want: []string{}},
				{after: time.Second, do: tick(), want: []string{"reset p1 (maximum lifetime reached)"}},
			}),
		},
		{
			name: "a task still running after the drain grace is failed and its pod reset",
			cfg:  withLifetime(10*time.Minute, time.Hour, 10*time.Minute), setup: two,
			steps: steps(firstTask, []step{
				{after: 70*time.Minute - time.Second, do: tick(), want: []string{}},
				{after: time.Second, do: tick(), want: []string{
					"fail t1 (the sandbox pod reached its maximum lifetime and the task ran past the drain grace period)",
					"end-lease t1 (maximum lifetime and drain grace passed)",
					"reset p1 (maximum lifetime and drain grace passed)",
				}},
				{do: tick(), want: []string{}},
				{do: request(pods("p1", "p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			}),
		},
		{
			name: "a binding whose key is unique has no maximum lifetime: it ends with its task",
			cfg: func(c *lending.Config) {
				withLifetime(10*time.Minute, time.Hour, 10*time.Minute)(c)
				c.Rule = lending.RuleTask
			},
			setup: two,
			steps: steps(firstTask, []step{
				{after: 3 * time.Hour, do: tick(), want: []string{}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)", "reset p1 after t1 (task ended: done)"}},
			}),
		},
		{
			name: "a maximum lifetime of 0 means none",
			cfg:  withLifetime(10*time.Minute, 0, 0), setup: two,
			steps: steps(firstTask, []step{
				{after: 48 * time.Hour, do: tick(), want: []string{}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
			}),
		},
	})
}

func removed(ws string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { return p.RemoveTenant(ws) }
}

func TestRemovedTenantResetsItsPods(t *testing.T) {
	two := setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"))
	firstTask := []step{
		{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
		{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
		{do: started("t1")},
	}
	run(t, []scenario{
		{
			name: "removing a tenant resets its bindings in hold",
			cfg:  withHold(time.Hour), setup: two,
			steps: steps(firstTask, []step{
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: removed("A"), want: []string{"reset p1 (tenant removed)"}},
			}),
		},
		{
			name: "a binding of a removed tenant is reset, not held, when its last task ends",
			cfg:  withHold(time.Hour), setup: two,
			steps: steps(firstTask, []step{
				{do: removed("A"), want: []string{}},
				{do: ended("t1", "tenant removed from the configuration"), want: []string{
					"end-lease t1 (tenant removed from the configuration)",
					"reset p1 after t1 (tenant removed)",
				}},
				{do: request(pods("p1", "p2"), "B"), want: []string{"claim B res=2 pod=p2"}},
			}),
		},
	})
}
