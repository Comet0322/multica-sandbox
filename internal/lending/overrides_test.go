package lending_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

// changed changes a served tenant's settings mid-scenario, as a reload of
// the manager's overrides does.
func changed(ws string, s lending.TenantSettings) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision {
		if err := p.SetTenant(ws, s); err != nil {
			panic(err)
		}
		return nil
	}
}

func inTenant(t lending.Task, ws string) lending.Task {
	t.Tenant = ws
	return t
}

func TestPerTenantOverrides(t *testing.T) {
	shortLife := lending.TenantSettings{MaxLifetime: durationPtr(30 * time.Minute)}
	run(t, []scenario{
		{
			name: "a tenant's own maximum lifetime applies to its bindings only",
			cfg:  withLifetime(10*time.Minute, time.Hour, 10*time.Minute),
			setup: setup(tenant("A", shortLife), tenant("B", unlimited),
				cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"), cleanPod("p3", "A", "B"), cleanPod("p4", "A", "B")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onIssue("a1", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "B"), want: []string{"claim B res=2 pod=p2"}},
				{do: claimed(2, inTenant(onIssue("b1", "is-2", "alice"), "B")), want: []string{"place b1 pod=p2 binding=2 new"}},
				{after: 30 * time.Minute, do: request(pods("p3"), "A"), want: []string{"claim A res=3 pod=p3"}},
				{do: claimed(3, onIssue("a2", "is-1", "alice")), want: []string{"place a2 pod=p3 binding=3 new"}},
				{do: request(pods("p4"), "B"), want: []string{"claim B res=4 pod=p4"}},
				{do: claimed(4, inTenant(onIssue("b2", "is-2", "alice"), "B")), want: []string{"place b2 pod=p2 binding=2", "release res=4 pod=p4"}},
			},
		},
		{
			name: "a tenant maximum lifetime of 0 means none, whatever the pool's",
			cfg:  withLifetime(10*time.Minute, time.Hour, 10*time.Minute),
			setup: setup(tenant("A", lending.TenantSettings{MaxLifetime: durationPtr(0)}),
				cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{after: 48 * time.Hour, do: tick(), want: []string{}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
			},
		},
		{
			name:  "a changed maximum lifetime applies to bindings opened afterwards; running tasks are not failed",
			cfg:   withLifetime(10*time.Minute, 2*time.Hour, 10*time.Minute),
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{after: 20 * time.Minute, do: changed("A", shortLife)},
				{after: 30 * time.Minute, do: tick(), want: []string{}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, onIssue("t3", "is-9", "alice")), want: []string{"place t3 pod=p2 binding=2 new"}},
				{do: started("t3")},
				{after: 40*time.Minute + time.Second, do: tick(), want: []string{
					"fail t3 (the sandbox pod reached its maximum lifetime and the task ran past the drain grace period)",
					"end-lease t3 (maximum lifetime and drain grace passed)",
					"reset p2 (maximum lifetime and drain grace passed)",
				}},
			},
		},
		{
			name: "a changed quota applies to the next claim",
			setup: setup(tenant("A", lending.TenantSettings{Quota: 1}),
				cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("t1", "A", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: changed("A", lending.TenantSettings{Quota: 2})},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			},
		},
		{
			name: "a changed affinity rule keys the next claimed task by the new rule",
			cfg:  withHold(10 * time.Minute),
			setup: setup(tenant("A", lending.TenantSettings{Rule: rulePersonIssue}),
				cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: changed("A", lending.TenantSettings{Rule: rulePerson})},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, onIssue("t2", "is-1", "alice")), want: []string{"place t2 pod=p2 binding=2 new"}},
			},
		},
	})
}
