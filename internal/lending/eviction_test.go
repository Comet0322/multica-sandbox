package lending_test

import (
	"testing"
	"time"
)

func TestEviction(t *testing.T) {
	two := setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"))
	three := setup(tenant("A", unlimited), cleanPod("p1", "A"), cleanPod("p2", "A"), cleanPod("p3", "A"))
	// p1 runs alice's is-1 and p2 bob's is-2.
	bothRunning := []step{
		{do: request(pods("p1"), "A"), want: []string{"claim A res=1 pod=p1"}},
		{do: claimed(1, onIssue("t1", "is-1", "alice")), want: []string{"place t1 pod=p1 binding=1 new"}},
		{do: started("t1")},
		{do: request(pods("p2"), "A"), want: []string{"claim A res=2 pod=p2"}},
		{do: claimed(2, onIssue("t2", "is-2", "bob")), want: []string{"place t2 pod=p2 binding=2 new"}},
		{do: started("t2")},
	}
	// Then both hold, p2 since longer.
	bothHeld := steps(bothRunning, []step{
		{after: time.Minute, do: ended("t2", "done"), want: []string{"end-lease t2 (done)"}},
		{after: time.Minute, do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
	})
	hold := withHold(time.Hour)
	run(t, []scenario{
		{
			name: "a hold is reserved only when the sandbox service has no free pod either",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: request(pods("p1", "p2"), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1", "p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
			}),
		},
		{
			name: "a task of another key ends the hold, the pod is handed back, and the task runs on the next pod acquired before any new claim",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: requestNoCapacity(pods("p1", "p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, onIssue("t3", "is-3", "carol")), want: []string{"reset p2 (evicted for task t3 of another affinity key)"}},
				{do: tick(), want: []string{}},
				{do: request(pods("p1"), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1"), "A"), want: []string{}},
				{do: acquired("p2", "A"), want: []string{"place t3 pod=p2 binding=3 new"}},
				{do: tick(), want: []string{}},
			}),
		},
		{
			name: "a reserved hold takes a task of its own key and is renewed",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, onIssue("t3", "is-2", "bob")), want: []string{"place t3 pod=p2 binding=2", "release res=3 pod=p2"}},
			}),
		},
		{
			name: "a task of another binding's key goes there, and the reserved hold is kept",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimed(3, onIssue("t3", "is-1", "alice")), want: []string{"place t3 pod=p1 binding=1", "release res=3 pod=p2"}},
				{after: time.Hour, do: tick(), want: []string{"reset p2 (hold expired)"}},
			}),
		},
		{
			name: "an empty claim against a hold leaves the hold in place",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
				{do: claimedNothing(3), want: []string{"release res=3 pod=p2"}},
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{"claim A res=4 pod=p2"}},
				{do: claimed(4, onIssue("t3", "is-2", "bob")), want: []string{"place t3 pod=p2 binding=2", "release res=4 pod=p2"}},
			}),
		},
		{
			name: "an idle pod is reserved before any hold",
			cfg:  hold, setup: three,
			steps: steps(bothHeld, []step{
				{do: request(pods("p1", "p2", "p3"), "A"), want: []string{"claim A res=3 pod=p3"}},
				{do: claimed(3, onIssue("t3", "is-3", "carol")), want: []string{"place t3 pod=p3 binding=3 new"}},
			}),
		},
		{
			name: "a hold is not reserved while a pod is free, even one not asking",
			cfg:  hold, setup: three,
			steps: steps(bothHeld, []step{
				{do: request(pods("p2"), "A"), want: []string{}},
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{}},
			}),
		},
		{
			name: "only the hold idle longest is reserved",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: requestNoCapacity(pods("p1"), "A"), want: []string{}},
				{do: requestNoCapacity(pods("p2"), "A"), want: []string{"claim A res=3 pod=p2"}},
			}),
		},
		{
			name: "a binding with a running task is never evicted, and the task stays unclaimed",
			cfg:  hold, setup: two,
			steps: steps(bothRunning, []step{
				{do: requestNoCapacity(pods("p1", "p2"), "A"), want: []string{}},
				{do: ended("t1", "done"), want: []string{"end-lease t1 (done)"}},
				{do: requestNoCapacity(pods("p1", "p2"), "A"), want: []string{"claim A res=3 pod=p1"}},
			}),
		},
		{
			name: "a task displaced from a lost pod evicts the hold idle longest only when no pod is free anywhere",
			cfg:  hold, setup: two,
			steps: steps(bothHeld, []step{
				{do: acquired("p3", "A"), want: []string{}},
				{do: request(pods("p3"), "A"), want: []string{"claim A res=3 pod=p3"}},
				{do: claimed(3, onIssue("t3", "is-3", "carol")), want: []string{"place t3 pod=p3 binding=3 new"}},
				{do: lost("p3"), want: []string{}},
				{do: request(pods("p1", "p2"), "A"), want: []string{"acquire"}},
				{do: requestNoCapacity(pods("p1", "p2"), "A"), want: []string{"reset p2 (evicted for task t3 of another affinity key)"}},
				{do: requestNoCapacity(pods("p1"), "A"), want: []string{}},
				{do: acquired("p2", "A"), want: []string{"place t3 pod=p2 binding=4 new"}},
			}),
		},
	})
}
