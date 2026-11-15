package lending_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

func tenantWide(s lending.TenantSettings) lending.TenantSettings {
	s.Rule = lending.RuleTenant
	return s
}

func TestTheTenantRule(t *testing.T) {
	// Two slots per pod, no hold and a short lifetime by default: the tenant
	// rule must keep its binding regardless.
	cfg := func(c *lending.Config) {
		c.Capacity, c.HoldTimeout, c.MaxLifetime = lending.Resources{Slots: 2}, 0, time.Minute
	}
	run(t, []scenario{
		{
			name:  "tasks of different people, and of nobody, share the tenant's binding",
			cfg:   cfg,
			setup: setup(tenant("A", tenantWide(unlimited)), cleanPod("p1", "A"), cleanPod("p2", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("t1", "A", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, lending.Task{ID: "t2", Tenant: "A"}), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p2"}},
			},
		},
		{
			name:  "its binding stays in hold past the hold timeout and the maximum lifetime, and takes the next task",
			cfg:   cfg,
			setup: setup(tenant("A", tenantWide(unlimited)), cleanPod("p1", "A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("t1", "A", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{after: 24 * time.Hour, do: tick(), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=2 pod=p1"}},
				{do: claimed(2, onTenant("t2", "A", "is-2", "bob")), want: []string{"place t2 pod=p1 binding=1", "release res=2 pod=p1"}},
			},
		},
		{
			name: "a kept binding in hold still goes to another tenant that needs the pod, beyond its guarantee",
			cfg:  cfg,
			setup: setup(tenant("A", tenantWide(unlimited)), tenant("B", unlimited), cleanPod("p1", "A", "B"),
				pendingSetup("B", queued("b1", 1, 0))),
			steps: []step{
				{do: requestLive(1, pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("t1", "A", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: requestLiveNoCapacity(1, pods("p1"), "B"), want: []string{"claim B res=2 pod=p1"}},
				{do: claimed(2, onTenant("b1", "B", "is-9", "bob")), want: []string{"reset p1 (evicted for task b1 of another affinity key)"}},
			},
		},
		{
			name: "within its tenant's guarantee it is never evicted",
			cfg:  cfg,
			setup: setup(tenant("A", tenantWide(guaranteed(1))), tenant("B", unlimited), cleanPod("p1", "A", "B"),
				pendingSetup("B", queued("b1", 1, 0))),
			steps: []step{
				{do: requestLive(1, pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onTenant("t1", "A", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
				{do: started("t1")},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: requestLiveNoCapacity(1, pods("p1"), "B"), want: []string{}},
			},
		},
	})
}
