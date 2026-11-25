package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

// The core writes what it holds to its store as it changes: the task's
// state, and what its pod serves.
func TestTheCoreRecordsItsPodsAndTasks(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: 10 * time.Minute}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	if task := r.store.task("t1"); task.State != core.TaskPlaced || task.PodID != "p1" || !task.ClaimedAt.Equal(t0) || len(task.Payload) == 0 || task.Email != "" {
		t.Fatalf("t1 once placed = %+v, want placed on p1 with its payload and no email", task)
	}
	if pod := r.store.pod("p1"); pod.Phase != policy.PhaseRunning || pod.Tenant != "A" || pod.Key != "issue:t1" || !pod.ReadyAt.Equal(t0) || !pod.GoneAt.IsZero() {
		t.Fatalf("p1 once placed = %+v", pod)
	}
	r.advance(time.Second)
	r.next("p1")
	if task := r.store.task("t1"); task.State != core.TaskHanded || !task.StartedAt.Equal(t0.Add(time.Second)) || len(task.Payload) == 0 {
		t.Fatalf("t1 once handed = %+v", task)
	}
	r.core.TaskRunning("p1", "t1")
	if task := r.store.task("t1"); task.State != core.TaskRunning || task.Payload != nil {
		t.Fatalf("t1 once running = %+v, want running with no payload", task)
	}
	r.core.TaskEnded("p1", "t1", "pod reported complete")
	if task := r.store.task("t1"); task.State != core.TaskEnded || task.EndReason != "pod reported complete" || task.FailPending {
		t.Fatalf("t1 once ended = %+v", task)
	}
	if pod := r.store.pod("p1"); pod.Phase != policy.PhaseHeld || !pod.Until.Equal(t0.Add(time.Second+10*time.Minute)) {
		t.Fatalf("p1 once held = %+v", pod)
	}
	r.advance(10 * time.Minute)
	r.core.Touch()
	if pod := r.store.pod("p1"); pod.Phase != policy.PhaseIdle || pod.Key != "" {
		t.Fatalf("p1 once idle = %+v", pod)
	}
	r.advance(24 * time.Hour)
	r.core.Touch()
	if pod := r.store.pod("p1"); pod.GoneAt.IsZero() || pod.GoneWhy == "" {
		t.Fatalf("p1 once deleted = %+v, want gone and why", pod)
	}
}

// A task a pod was handed is handed to it again until the pod reports it
// started, so a lost response costs nothing.
func TestAHandedTaskIsHandedAgainUntilItStarts(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	for i := range 2 {
		if got := r.next("p1"); got != "t1" {
			t.Fatalf("poll %d: p1 got %q, want t1", i, got)
		}
	}
	r.core.TaskRunning("p1", "t1")
	if got := r.next("p1"); got != "" {
		t.Fatalf("p1 got %q once t1 runs, want nothing", got)
	}
	if l := r.core.Leases(); len(l) != 1 {
		t.Fatalf("leases = %+v, want one", l)
	}
}

// A restarted core takes over from the store: the running task runs on and
// is not handed again; the task placed on a pod and the one handed to a pod
// that never started it are handed to their pods; the hold resumes and the
// idle pod is kept; a pod the record does not hold is deleted. Nothing is
// failed.
func TestARestartedCoreCarriesOnFromItsStore(t *testing.T) {
	r := newRig(t, core.Config{HoldTimeout: 10 * time.Minute}, tenant("A", policy.TenantSettings{}))
	run := func(pod, task, key string) {
		r.pool.add(pod, "A")
		r.source.addTask("A", policy.KindPool, task, key, "bob", 0)
		r.dispatch()
	}
	run("p-run", "t1", "issue:1")
	r.next("p-run")
	r.core.TaskRunning("p-run", "t1")
	run("p-held", "t2", "issue:2")
	r.next("p-held")
	run("p-idle", "t3", "")
	r.next("p-idle")
	run("p-placed", "t4", "issue:4")
	run("p-handed", "t5", "issue:5")
	r.next("p-handed")
	r.core.TaskEnded("p-held", "t2", "pod reported complete")
	r.core.TaskEnded("p-idle", "t3", "pod reported complete")
	r.pool.add("p-warm", "A")

	r.advance(time.Minute)
	r.restart()

	if got := r.pool.ids(); !slices.Equal(got, []string{"p-run", "p-held", "p-idle", "p-placed", "p-handed"}) {
		t.Fatalf("pods after the restart = %v, want the five the record holds", got)
	}
	if !r.core.Owns("p-run", "t1") {
		t.Fatal("the running task is not its pod's")
	}
	if got := r.next("p-run"); got != "" {
		t.Fatalf("the running task's pod was handed %q", got)
	}
	if got := r.next("p-placed"); got != "t4" {
		t.Fatalf("the placed task's pod got %q, want t4", got)
	}
	if got := r.next("p-handed"); got != "t5" {
		t.Fatalf("the handed task's pod got %q, want t5 again", got)
	}
	leases := r.core.Leases()
	i := slices.IndexFunc(leases, func(l core.LeaseInfo) bool { return l.TaskID == "t1" })
	if len(leases) != 3 || i < 0 || !leases[i].StartedAt.Equal(t0) {
		t.Fatalf("leases = %+v, want t1 since it started, t5 and t4", leases)
	}
	// The next task of the held key lands on its held pod; another key takes
	// the idle pod.
	r.source.addTask("A", policy.KindPool, "t6", "issue:2", "bob", 0)
	r.dispatch()
	if got := r.next("p-held"); got != "t6" {
		t.Fatalf("the held pod picked up %q, want t6", got)
	}
	r.source.addTask("A", policy.KindPool, "t7", "issue:7", "bob", 0)
	r.dispatch()
	if got := r.next("p-idle"); got != "t7" {
		t.Fatalf("the idle pod picked up %q, want t7", got)
	}
	r.core.TaskEnded("p-run", "t1", "pod reported complete")
	if pod := r.store.pod("p-run"); pod.Phase != policy.PhaseHeld || pod.Key != "issue:1" {
		t.Fatalf("the running pod after its task = %+v, want held for issue:1", pod)
	}
	if _, fails, _ := r.source.snapshot(); len(fails) != 0 {
		t.Fatalf("fails = %v, want none", fails)
	}
}

// A pod Kubernetes lost while the manager was down is found lost at the first
// dispatch: its started task is failed, and the task placed on it goes to
// another pod.
func TestARestoredPodThatIsGoneIsLost(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.pool.add("p2", "A")
	r.source.add("A", "t2", "alice", 0)
	r.dispatch()
	r.pool.lose("p1")
	r.pool.lose("p2")
	r.restart()
	r.pool.add("p3", "A")
	r.dispatch()
	_, fails, _ := r.source.snapshot()
	if !slices.Equal(fails, []string{"A t1 the pod running this task was lost"}) {
		t.Fatalf("fails = %v, want t1's", fails)
	}
	if got := r.next("p3"); got != "t2" {
		t.Fatalf("p3 got %q, want t2, whose pod was lost before it started", got)
	}
}

// A restored pod's task fails with the pod's own reason when the pod turns
// out lost.
func TestARestoredPodLostFailsItsTask(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("run", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("run")
	r.restart()
	r.pool.lose("run")
	r.core.PodLost("run", "the pod ran out of memory (limit 4Gi)")
	_, fails, _ := r.source.snapshot()
	if !slices.Equal(fails, []string{"A t1 the pod running this task was lost: the pod ran out of memory (limit 4Gi)"}) {
		t.Fatalf("fails = %v", fails)
	}
}

// A failure report the source had not taken when the manager stopped is
// sent again by the restarted one.
func TestAFailureReportPendingAtARestartIsSentAgain(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.source.failWith(errors.New("upstream unavailable"))
	r.pool.lose("p1")
	r.core.PodLost("p1", "the pod was deleted")
	if task := r.store.task("t1"); task.State != core.TaskEnded || !task.FailPending {
		t.Fatalf("t1 = %+v, want ended with its failure pending", task)
	}
	r.source.failWith(nil)
	r.restart()
	if n := r.core.PendingFailReports(); n != 0 {
		t.Fatalf("pending reports = %d once the restarted core sent it", n)
	}
	if task := r.store.task("t1"); task.FailPending {
		t.Fatalf("t1 = %+v, want its failure no longer pending", task)
	}
	_, fails, _ := r.source.snapshot()
	if len(fails) != 2 || fails[0] != fails[1] {
		t.Fatalf("fails = %v, want the same report before and after the restart", fails)
	}
}

// A record that does not add up is not taken over: a pod said to run a
// task the record does not hold is deleted, a task handed to a pod the
// record no longer holds is failed, and a task placed on such a pod waits
// for another.
func TestARestartedCoreDropsWhatTheRecordCannotVouchFor(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	ctx := context.Background()
	pods := []core.PodRow{{ID: "p-odd", Phase: policy.PhaseRunning, Tenant: "A", Kind: policy.KindPool, ReadyAt: t0}}
	tasks := []core.TaskRow{
		{Task: core.Task{Task: policy.Task{ID: "t-handed", Tenant: "A", Kind: policy.KindPool}, Payload: json.RawMessage(`{"id":"t-handed"}`)}, PodID: "p-gone", State: core.TaskHanded, ClaimedAt: t0},
		{Task: core.Task{Task: policy.Task{ID: "t-placed", Tenant: "A", Kind: policy.KindPool}, Payload: json.RawMessage(`{"id":"t-placed"}`)}, PodID: "p-gone", State: core.TaskPlaced, ClaimedAt: t0},
	}
	if err := r.store.Write(ctx, pods, tasks); err != nil {
		t.Fatal(err)
	}
	r.pool.add("p-odd", "A")
	r.restart()
	if got := r.pool.ids(); len(got) != 0 {
		t.Fatalf("pods after the restart = %v, want p-odd deleted", got)
	}
	if pod := r.store.pod("p-odd"); pod.GoneAt.IsZero() {
		t.Fatalf("p-odd = %+v, want gone", pod)
	}
	if _, fails, _ := r.source.snapshot(); !slices.Equal(fails, []string{"A t-handed the pod running this task was lost"}) {
		t.Fatalf("fails = %v, want the handed task's", fails)
	}
	r.pool.add("p1", "A")
	r.dispatch()
	if got := r.next("p1"); got != "t-placed" {
		t.Fatalf("p1 got %q, want the placed task", got)
	}
}

// A task Multica hands out again, its earlier delivery not started in time,
// replaces the delivery the core holds and keeps its place.
func TestATaskClaimedAgainReplacesItsDelivery(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.pool.add("p2", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	again := core.Task{Task: policy.Task{ID: "t1", Tenant: "A", Kind: policy.KindPool, Key: "issue:t1"}, Payload: json.RawMessage(`{"id":"t1","delivery":2}`)}
	r.source.mu.Lock()
	r.source.queues["A"] = append(r.source.queues["A"], again)
	r.source.mu.Unlock()
	r.dispatch()
	if got := string(r.core.Next(context.Background(), "p1")); !strings.Contains(got, `"delivery":2`) {
		t.Fatalf("p1 got %s, want the new delivery", got)
	}
	if got := r.next("p2"); got != "" {
		t.Fatalf("p2 got %q, want nothing: the task kept its place", got)
	}
}
