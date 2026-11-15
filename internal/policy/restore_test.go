package policy_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/policy"
)

// restored is a pod a previous run left, as the caller found it.
func restored(id string, info policy.PodInfo) func(*policy.Policy) {
	return func(p *policy.Policy) { p.PodRestored(id, info) }
}

func TestRestoredPods(t *testing.T) {
	run(t, []scenario{
		{
			name: "a restored running task runs on, and its pod is held for its key once it ends",
			setup: setup(tenant("A", plain), restored("p1", policy.PodInfo{
				Phase: policy.PhaseRunning, Tenant: "A", Kind: policy.KindPool, Key: "issue:1", Task: "a1",
			})),
			steps: []step{
				{do: info("p1"), want: []string{"running A/pool key=issue:1"}},
				{do: busy("A"), want: []string{"busy"}},
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: info("p1"), want: []string{"held A/pool key=issue:1 until 09:10"}},
			},
		},
		{
			name: "a restored hold resumes with its remaining time and takes its key's next task",
			setup: setup(tenant("A", plain), warmPod("p2", "A"), restored("p1", policy.PodInfo{
				Phase: policy.PhaseHeld, Tenant: "A", Kind: policy.KindPool, Key: "issue:1", Since: t0.Add(-5 * time.Minute), Until: t0.Add(5 * time.Minute),
			})),
			steps: steps(runs(1, "p2", poolTask("a2", "A", "issue:1"), "p1"), []step{
				{do: ended("a2"), want: []string{"end-lease a2 (done)"}},
				{after: 4 * time.Minute, do: tick()},
				{do: info("p1"), want: []string{"held A/pool key=issue:1 until 09:10"}},
			}),
		},
		{
			name: "a restored clean hold ends with its pod deleted at its own end",
			setup: setup(tenant("A", plain), restored("p1", policy.PodInfo{
				Phase: policy.PhaseHeld, Tenant: "A", Kind: policy.KindClean, Key: "issue:1", Since: t0.Add(-5 * time.Minute), Until: t0.Add(5 * time.Minute),
			})),
			steps: []step{
				{after: 4 * time.Minute, do: tick()},
				{after: time.Minute, do: tick(), want: []string{"delete p1 (hold expired)"}},
			},
		},
		{
			name: "a restored idle pod takes another key of its tenant, and ages out from when it became idle",
			setup: setup(tenant("A", plain), warmPod("p2", "A"), restored("p1", policy.PodInfo{
				Phase: policy.PhaseIdle, Tenant: "A", Kind: policy.KindPool, Since: t0.Add(-23 * time.Hour),
			}), restored("p3", policy.PodInfo{
				Phase: policy.PhaseIdle, Tenant: "A", Kind: policy.KindPool, Since: t0.Add(-1 * time.Hour),
			})),
			steps: steps(runs(1, "p3", poolTask("a1", "A", "issue:9"), "p3"), []step{
				{after: time.Hour, do: tick(), want: []string{"delete p1 (idle for its maximum idle lifetime)"}},
			}),
		},
		{
			name:  "a restored pod never serves another tenant",
			setup: setup(tenant("A", plain), tenant("B", plain), warmPod("p2", "A", "B"), restored("p1", policy.PodInfo{Phase: policy.PhaseIdle, Tenant: "A", Kind: policy.KindPool, Since: t0})),
			steps: runs(1, "p2", poolTask("b1", "B", ""), "p2"),
		},
	})
}

func busy(tenant string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string {
		if p.Busy(tenant) {
			return []string{"busy"}
		}
		return nil
	}
}
