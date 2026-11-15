package policy_test

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"sandbox-manager/internal/policy"
)

// Over many random events, the decisions never give a pod to another
// tenant, never give a clean pod another key, and never give a pod a second
// task while one runs.
func TestRandomEventsKeepThePodRules(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { randomRun(t, seed) })
	}
}

func randomRun(t *testing.T, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed))
	now := t0
	p, err := policy.New(policy.Config{Now: func() time.Time { return now }, HoldTimeout: 10 * time.Minute, MaxIdle: time.Hour, MaxPods: 6})
	if err != nil {
		t.Fatal(err)
	}
	tenants := []string{"A", "B", "C"}
	for i, id := range tenants {
		_ = p.SetTenant(id, policy.TenantSettings{Guarantee: i % 2, Weight: i + 1})
	}
	keys := []string{"", "issue:1", "issue:2", "chat:3"}
	kinds := []policy.Kind{policy.KindPool, policy.KindClean}

	type podRecord struct {
		tenant  string
		kind    policy.Kind
		key     string
		running string
	}
	pods := map[string]*podRecord{}
	live := map[string]bool{}
	taskPod := map[string]string{}
	started := map[string]bool{}
	claims := map[policy.ClaimID]policy.Claim{}
	taskMeta := map[string]policy.Task{}
	tasks, places := 0, 0
	podSeq := 0

	check := func(ds []policy.Decision) {
		t.Helper()
		for _, d := range ds {
			switch d := d.(type) {
			case policy.Claim:
				claims[d.ID] = d
			case policy.Place:
				task := d.Task
				rec := pods[d.Pod]
				meta := taskMeta[task]
				if rec.running != "" {
					t.Fatalf("pod %s given %s while it runs %s", d.Pod, task, rec.running)
				}
				if rec.tenant != "" && rec.tenant != meta.Tenant {
					t.Fatalf("pod %s of %s given a task of %s", d.Pod, rec.tenant, meta.Tenant)
				}
				if rec.kind != "" && rec.kind != meta.Kind {
					t.Fatalf("pod %s of kind %s given a %s task", d.Pod, rec.kind, meta.Kind)
				}
				if rec.kind == policy.KindClean && rec.key != meta.Key {
					t.Fatalf("clean pod %s of key %q given key %q", d.Pod, rec.key, meta.Key)
				}
				if rec.kind == policy.KindClean && meta.Key == "" {
					t.Fatalf("clean pod %s reused for a task with no key", d.Pod)
				}
				rec.tenant, rec.kind, rec.key, rec.running = meta.Tenant, meta.Kind, meta.Key, task
				taskPod[task] = d.Pod
				places++
			case policy.Delete:
				if rec := pods[d.Pod]; rec != nil && rec.running != "" {
					t.Fatalf("pod %s deleted while it runs %s", d.Pod, rec.running)
				}
				delete(live, d.Pod)
			case policy.EndLease:
				if pd, ok := taskPod[d.Task]; ok {
					pods[pd].running = ""
					delete(taskPod, d.Task)
				}
			case policy.Fail:
				if pd, ok := taskPod[d.Task]; ok {
					pods[pd].running = ""
					delete(taskPod, d.Task)
				}
			}
		}
	}

	for i := 0; i < 1000; i++ {
		switch rng.IntN(8) {
		case 0:
			if len(live) < 6 {
				podSeq++
				id := fmt.Sprintf("p%d", podSeq)
				pods[id], live[id] = &podRecord{}, true
				check(p.PodAcquired(id, tenants...))
			}
		case 1, 2:
			check(p.RequestClaim(policy.ClaimRequest{
				Tenants: tenants, Kinds: kinds, LivePods: len(live), NoCapacity: rng.IntN(2) == 0, Starting: rng.IntN(2),
			}))
		case 3:
			for res, c := range claims {
				delete(claims, res)
				if rng.IntN(4) == 0 {
					check(p.ClaimReturned(res, nil, false))
					break
				}
				tasks++
				tk := policy.Task{ID: fmt.Sprintf("t%d", tasks), Tenant: c.Tenant, Kind: c.Kind, Key: keys[rng.IntN(len(keys))]}
				taskMeta[tk.ID] = tk
				check(p.ClaimReturned(res, &tk, false))
				break
			}
		case 4:
			for task := range taskPod {
				if !started[task] {
					started[task] = true
					p.TaskStarted(task)
				}
				break
			}
		case 5:
			for task := range taskPod {
				if started[task] {
					check(p.TaskEnded(task, "done"))
					break
				}
			}
		case 6:
			if rng.IntN(5) == 0 {
				for id := range live {
					delete(live, id)
					check(p.PodLost(id, "lost", "lost"))
					break
				}
			}
		case 7:
			now = now.Add(time.Duration(rng.IntN(20)) * time.Minute)
			check(p.Tick())
		}
	}
	if places < 20 {
		t.Fatalf("only %d tasks placed: the scenario exercises too little", places)
	}
}
