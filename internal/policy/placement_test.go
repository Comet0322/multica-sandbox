package policy_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/policy"
)

func TestPoolPlacement(t *testing.T) {
	run(t, []scenario{
		{
			name:  "a pool task runs on a warm pod, which is held for its key after it",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: info("p1"), want: []string{"held A/pool key=issue:1 until 09:10"}},
			}),
		},
		{
			name:  "the next task of the key goes to its held pod, and the warm pod readied for the claim stays warm",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
			}, runs(2, "p2", poolTask("a2", "A", "issue:1"), "p1"), []step{
				{do: info("p2"), want: []string{"warm"}},
				{do: info("p1"), want: []string{"running A/pool key=issue:1"}},
			}),
		},
		{
			name:  "a held pod is not another key's while its hold lasts",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
			}, runs(2, "p2", poolTask("a2", "A", "issue:2"), "p2")),
		},
		{
			name:  "once its hold expired, a pool runtime pod is idle and takes another key of its tenant before a warm pod",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{after: 10 * time.Minute, do: tick()},
				{do: info("p1"), want: []string{"idle A/pool since 09:10"}},
			}, runs(2, "p1", poolTask("a2", "A", "issue:2"), "p1")),
		},
		{
			name:  "an idle pod is never another tenant's",
			setup: setup(tenant("A", plain), tenant("B", plain), warmPod("p1", "A", "B"), warmPod("p2", "A", "B")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{after: 10 * time.Minute, do: tick()},
			}, runs(2, "p2", poolTask("b1", "B", "issue:2"), "p2")),
		},
		{
			name:  "a task with no key is never held: its pool runtime pod is idle at once",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: info("p1"), want: []string{"idle A/pool since 09:00"}},
			}),
		},
		{
			name:  "with hold timeout zero a pool runtime pod is idle at once",
			cfg:   func(c *policy.Config) { c.HoldTimeout = 0 },
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: info("p1"), want: []string{"idle A/pool since 09:00"}},
			}),
		},
		{
			name:  "a tenant's own hold timeout applies to its pods",
			setup: setup(tenant("A", policy.TenantSettings{HoldTimeout: durationPtr(time.Minute)}), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: info("p1"), want: []string{"held A/pool key=issue:1 until 09:01"}},
			}),
		},
		{
			name:  "an idle pod is deleted after its maximum idle lifetime",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{after: 24*time.Hour - time.Second, do: tick()},
				{after: time.Second, do: tick(), want: []string{"delete p1 (idle for its maximum idle lifetime)"}},
				{do: info("p1"), want: []string{"unknown"}},
			}),
		},
		{
			name:  "one task at a time: a key's pod running a task is not given a second one",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", "issue:1"), "p1"), runs(2, "p2", poolTask("a2", "A", "issue:1"), "p2")),
		},
	})
}

func TestCleanPlacement(t *testing.T) {
	run(t, []scenario{
		{
			name:  "a clean task runs on a warm pod, held for its key after it",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", cleanTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{do: info("p1"), want: []string{"held A/clean key=issue:1 until 09:10"}},
			}),
		},
		{
			name:  "the next clean task of the key goes to its held pod",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", cleanTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
			}, runs(2, "p2", cleanTask("a2", "A", "issue:1"), "p1")),
		},
		{
			name:  "a clean pod's hold expiring deletes it",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", cleanTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
				{after: 10 * time.Minute, do: tick(), want: []string{"delete p1 (hold expired)"}},
			}),
		},
		{
			name:  "a clean task with no key has its pod deleted when it ends",
			setup: setup(tenant("A", plain), warmPod("p1", "A")),
			steps: steps(runs(1, "p1", cleanTask("a1", "A", ""), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)", "delete p1 after a1 (task ended: done)"}},
			}),
		},
		{
			name:  "a clean task never takes an idle pool runtime pod",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", poolTask("a1", "A", ""), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
			}, runs(2, "p2", cleanTask("a2", "A", "issue:9"), "p2")),
		},
		{
			name:  "a pool task never takes a clean pod held for its key",
			setup: setup(tenant("A", plain), warmPod("p1", "A"), warmPod("p2", "A")),
			steps: steps(runs(1, "p1", cleanTask("a1", "A", "issue:1"), "p1"), []step{
				{do: ended("a1"), want: []string{"end-lease a1 (done)"}},
			}, runs(2, "p2", poolTask("a2", "A", "issue:1"), "p2")),
		},
	})
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// info describes a pod as the policy reports it.
func info(pod string) func(*policy.Policy) []string {
	return func(p *policy.Policy) []string {
		pi, ok := p.Pod(pod)
		if !ok {
			return []string{"unknown"}
		}
		s := string(pi.Phase)
		if pi.Tenant != "" {
			s += " " + pi.Tenant + "/" + string(pi.Kind)
		}
		if pi.Key != "" {
			s += " key=" + pi.Key
		}
		switch pi.Phase {
		case policy.PhaseHeld:
			s += " until " + pi.Until.Format("15:04")
		case policy.PhaseIdle:
			s += " since " + pi.Since.Format("15:04")
		}
		return []string{s}
	}
}
