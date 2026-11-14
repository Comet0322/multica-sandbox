package lending_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/lending"
)

// t0 is the scenario clock's start; pending tasks are dated relative to it.
var t0 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// queued is a pending task waiting to be claimed, created at t0+age.
func queued(id string, priority int, age time.Duration) lending.Pending {
	return lending.Pending{ID: id, Priority: priority, CreatedAt: t0.Add(age), Status: "queued"}
}

// pending records a tenant's pending list as read on a dispatch tick.
func pending(ws string, tasks ...lending.Pending) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { p.PendingChanged(ws, tasks); return nil }
}

func pendingSetup(ws string, tasks ...lending.Pending) func(*lending.Policy) {
	return func(p *lending.Policy) { p.PendingChanged(ws, tasks) }
}

func pendingUnknown(ws string) func(*lending.Policy) []lending.Decision {
	return func(p *lending.Policy) []lending.Decision { p.PendingUnknown(ws); return nil }
}

func TestGlobalQueue(t *testing.T) {
	run(t, []scenario{
		{
			name: "a higher-priority task in one tenant is claimed before an older lower-priority task in another",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0)), pendingSetup("B", queued("b1", 3, time.Minute))),
			steps: []step{
				{do: request(pods("p1"), "A", "B"), want: []string{"claim B res=1 pod=p1"}},
			},
		},
		{
			name: "within the same priority the oldest task is claimed first, then the next one",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 2, time.Minute)), pendingSetup("B", queued("b1", 2, 0))),
			steps: []step{
				{do: request(pods("p1"), "A", "B"), want: []string{"claim B res=1 pod=p1"}},
				{do: request(pods("p2"), "A", "B"), want: []string{"claim A res=2 pod=p2"}},
			},
		},
		{
			name: "only queued tasks are ordered; dispatched ones are already someone's",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"),
				pendingSetup("A", lending.Pending{ID: "a1", Priority: 9, CreatedAt: t0, Status: "dispatched"}, queued("a2", 1, time.Hour)),
				pendingSetup("B", queued("b1", 2, time.Minute))),
			steps: []step{
				{do: request(pods("p1"), "A", "B"), want: []string{"claim B res=1 pod=p1"}},
			},
		},
		{
			name: "a tenant at its quota is skipped and the next eligible tenant is claimed instead",
			setup: setup(tenant("A", lending.TenantSettings{Quota: 1}), tenant("B", unlimited),
				cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B")),
			steps: []step{
				{do: pending("A", queued("a1", 5, 0), queued("a2", 5, time.Second))},
				{do: pending("B", queued("b1", 1, 0))},
				{do: request(pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("a1", "A", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A", "B"), want: []string{"claim B res=2 pod=p2"}},
			},
		},
		{
			name: "quota counts the tenant's running tasks, not the room left in their binding",
			cfg:  shared(3),
			setup: setup(tenant("A", lending.TenantSettings{Quota: 2}), cleanPod("p1", "A"), cleanPod("p2", "A"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second), queued("a3", 1, 2*time.Second))),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("a1", "A", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: claimed(2, task("a2", "A", "alice")), want: []string{"place a2 pod=p1 binding=1", "release res=2 pod=p2"}},
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: ended("a1", "done"), want: []string{"end-lease a1 (done)"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
			},
		},
		{
			name: "a binding in hold does not count against its tenant's quota",
			cfg:  withHold(10 * time.Minute),
			setup: setup(tenant("A", lending.TenantSettings{Quota: 1}), cleanPod("p1", "A"), cleanPod("p2", "A"),
				pendingSetup("A", queued("a1", 1, 0), queued("a2", 1, time.Second))),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, onIssue("a1", "is-1", "alice")), want: []string{"place a1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: ended("a1", "done"), want: []string{"end-lease a1 (done)"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			},
		},
		{
			name:  "tasks no pod can take stay queued upstream and no claim is sent",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A"), cleanPod("p2", "B")),
			steps: steps(leased("p1", "t1", 1), []step{
				{do: pending("A", queued("a1", 9, 0))},
				// p1 is busy, and p2 does not serve A: a pod is asked for, and
				// with none to acquire nothing is claimed.
				{do: request(pods("p1"), "A", "B"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1"), "A", "B"), want: []string{}},
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{}},
			}),
		},
		{
			name: "a claim that returns a different task of the tenant is accepted",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"),
				pendingSetup("A", queued("a1", 1, 0))),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimed(1, task("a9", "A", "alice")), want: []string{"place a9 pod=p1 binding=1 new"}},
			},
		},
		{
			name: "an empty claim stops claims from the tenant until its next pending list",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"),
				pendingSetup("A", queued("a1", 1, 0))),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: claimedNothing(1), want: []string{"release res=1 pod=p1"}},
				{do: request(pods("p1"), "A"), want: []string{}},
				{do: pending("A", queued("a1", 1, 0))},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=2 pod=p1"}},
			},
		},
		{
			name:  "a tenant with quota and an empty pending list is claimed from once per pending list, so the server can promote deferred retries",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"), pendingSetup("A")),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: claimed(1, task("r1", "A", "alice")), want: []string{"place r1 pod=p1 binding=1 new"}},
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: pending("A")},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
			},
		},
		{
			name: "a queued task seen once is claimed once, plus at most one extra claim until the next pending list",
			setup: setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"), cleanPod("p3", "A"),
				pendingSetup("A", queued("a1", 1, 0))),
			steps: []step{
				{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
				{do: request(pods("p3"), "A"), want: []string{}},
			},
		},
		{
			name: "a tenant whose pending list is unknown is claimed from in turn, as before",
			setup: setup(tenant("A", unlimited), tenant("B", unlimited), cleanPod("p1", "A", "B"), cleanPod("p2", "A", "B"),
				pendingSetup("A", queued("a1", 1, 0))),
			steps: []step{
				{do: request(pods("p1"), "A", "B"), want: []string{"claim A res=1 pod=p1"}},
				{do: request(pods("p2"), "A", "B"), want: []string{"claim B res=2 pod=p2"}},
				{do: pendingUnknown("A")},
				{do: claimedNothing(1), want: []string{"release res=1 pod=p1"}},
				{do: request(pods("p1"), "A"), want: []string{"claim A res=3 pod=p1"}},
			},
		},
	})
}
