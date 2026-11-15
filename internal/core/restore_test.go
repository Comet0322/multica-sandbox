package core_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

func TestTheCoreRecordsWhatEachPodServes(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: 10 * time.Minute}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	if rec := r.pool.record("p1"); rec.Phase != policy.PhaseRunning || rec.Task != "t1" || !rec.TaskStarted.IsZero() {
		t.Fatalf("record once placed = %+v, want t1 running, not started", rec)
	}
	r.next("p1")
	if rec := r.pool.record("p1"); rec.Tenant != "A" || rec.Kind != policy.KindPool || rec.Key != "issue:t1" || !rec.TaskStarted.Equal(t0) {
		t.Fatalf("record once started = %+v", rec)
	}
	r.core.TaskEnded("p1", "t1", "pod reported complete")
	if rec := r.pool.record("p1"); rec.Phase != policy.PhaseHeld || rec.Task != "" || !rec.Until.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("record once held = %+v", rec)
	}
	r.advance(10 * time.Minute)
	r.core.Touch()
	if rec := r.pool.record("p1"); rec.Phase != policy.PhaseIdle || rec.Key != "" || !rec.Since.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("record once idle = %+v", rec)
	}
}

// A restarted core takes over the pods a previous run left: the running
// task runs on, the hold resumes, the idle pod is kept, and a task the pod
// never took up is failed with its pod deleted.
func TestARestartedCoreCarriesOn(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: 10 * time.Minute}, tenant("A", policy.TenantSettings{}))
	for _, id := range []string{"run", "held", "idle", "placed"} {
		r.pool.restore(id, "A")
	}
	r.pool.add("warm", "A")
	r.core.Restore([]core.RestoredPod{
		{ID: "run", Record: core.PodRecord{Phase: policy.PhaseRunning, Tenant: "A", Kind: policy.KindPool, Key: "issue:1", Task: "t1", TaskStarted: t0.Add(-time.Hour)}},
		{ID: "held", Record: core.PodRecord{Phase: policy.PhaseHeld, Tenant: "A", Kind: policy.KindPool, Key: "issue:2", Since: t0.Add(-5 * time.Minute), Until: t0.Add(5 * time.Minute)}},
		{ID: "idle", Record: core.PodRecord{Phase: policy.PhaseIdle, Tenant: "A", Kind: policy.KindPool, Since: t0.Add(-time.Hour)}},
		{ID: "placed", Record: core.PodRecord{Phase: policy.PhaseRunning, Tenant: "A", Kind: policy.KindPool, Key: "issue:3", Task: "t3"}},
	})

	if !r.core.Owns("run", "t1") || !r.core.TenantBusy("A") {
		t.Fatal("the running task is not the restored pod's")
	}
	if l := r.core.Leases(); len(l) != 1 || l[0].TaskID != "t1" || !l[0].StartedAt.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("leases = %+v, want t1 since it started", l)
	}
	_, fails, _ := r.source.snapshot()
	if len(fails) != 1 || !strings.HasPrefix(fails[0], "A t3 ") || !strings.Contains(fails[0], "restarted") {
		t.Fatalf("fails = %v, want t3 failed: its pod never took it up", fails)
	}
	if _, backs := r.pool.snapshot(); len(backs) != 1 || !strings.HasPrefix(backs[0], "placed ") {
		t.Fatalf("hand-backs = %v, want the pod of the task never taken up", backs)
	}

	// The next task of the held key lands on its held pod.
	r.source.addTask("A", policy.KindPool, "t2", "issue:2", "bob", 0)
	r.dispatch()
	if got := r.next("held"); got != "t2" {
		t.Fatalf("the held pod picked up %q, want t2", got)
	}
	// Another key takes the idle pod before the warm one.
	r.source.addTask("A", policy.KindPool, "t4", "issue:4", "bob", 0)
	r.dispatch()
	if got := r.next("idle"); got != "t4" {
		t.Fatalf("the idle pod picked up %q, want t4", got)
	}
	// The running task reports through its pod and ends as before.
	r.core.TaskEnded("run", "t1", "pod reported complete")
	if rec := r.pool.record("run"); rec.Phase != policy.PhaseHeld || rec.Key != "issue:1" {
		t.Fatalf("record of the restored pod after its task = %+v", rec)
	}
}

// A restored pod's task fails with the pod's own reason when the pod turns
// out lost.
func TestARestoredPodLostFailsItsTask(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.restore("run", "A")
	r.core.Restore([]core.RestoredPod{{ID: "run", Record: core.PodRecord{Phase: policy.PhaseRunning, Tenant: "A", Kind: policy.KindPool, Task: "t1", TaskStarted: t0}}})
	r.pool.lose("run")
	r.core.PodLost("run", "the pod ran out of memory (limit 4Gi)")
	_, fails, _ := r.source.snapshot()
	if !slices.Equal(fails, []string{"A t1 the pod running this task was lost: the pod ran out of memory (limit 4Gi)"}) {
		t.Fatalf("fails = %v", fails)
	}
}
