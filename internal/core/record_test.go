package core_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

// A slow database holds up only what must be in the record first. A task's
// reports, a heartbeat, the inventory, a lost pod and a pod with nothing to
// pick up go on while a write hangs; the record catches up once it lands.
func TestASlowStoreHoldsUpOnlyWhatMustBeWrittenFirst(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.pool.add("p2", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.core.TaskRunning("p1", "t1")
	r.source.add("A", "t2", "alice", 0)
	r.dispatch()
	r.next("p2")
	r.store.holdWrites()
	t.Cleanup(r.store.releaseWrites)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.core.TaskEnded("p1", "t1", "pod reported complete")
		r.core.Touch()
		r.core.View()
		r.core.TaskRunning("p2", "t2")
		r.pool.lose("p2")
		r.core.PodLost("p2", "the pod was deleted")
		r.next("p1")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reports, a heartbeat, the inventory, a lost pod and a poll waited for the database")
	}
	r.store.releaseWrites()
	eventually(t, "t1 ended in the record", func() bool { return r.store.task("t1").State == core.TaskEnded })
	eventually(t, "t2's failure reported", func() bool {
		_, fails, _ := r.source.snapshot()
		return slices.Equal(fails, []string{"A t2 the pod running this task was lost: the pod was deleted"})
	})
}

// A failure is reported only once the record holds it pending, even when it
// is decided while the write of another change is under way.
func TestAFailureIsReportedOnlyOnceTheRecordHoldsIt(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.pool.add("p2", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.core.TaskRunning("p1", "t1")
	r.source.add("A", "t2", "alice", 0)
	r.dispatch()
	r.next("p2")
	var mu sync.Mutex
	var unrecorded []string
	r.source.whenFail(func(id string) {
		if !r.store.task(id).FailPending {
			mu.Lock()
			unrecorded = append(unrecorded, id)
			mu.Unlock()
		}
	})
	r.store.holdWrites()
	t.Cleanup(r.store.releaseWrites)

	go r.core.TaskRunning("p2", "t2")
	r.store.writeStarted(t)
	go func() {
		r.pool.lose("p1")
		r.core.PodLost("p1", "the pod was deleted")
	}()
	eventually(t, "t1's failure decided", func() bool { return r.core.PendingFailReports() == 1 })
	r.store.letWrite()
	r.store.releaseWrites()
	eventually(t, "t1's failure reported", func() bool {
		_, fails, _ := r.source.snapshot()
		return len(fails) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if len(unrecorded) != 0 {
		t.Fatalf("reported %v before the record held the failure pending", unrecorded)
	}
}

// A pod is handed a task only once the record says so, and until then the
// task is not the pod's: when the pod is lost meanwhile, the task goes to
// another pod instead of being failed.
func TestATaskTheRecordDoesNotSayHandedIsNotStarted(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.store.failWith(errors.New("connection refused"))
	if got := r.next("p1"); got != "" {
		t.Fatalf("p1 was handed %q, which the record does not say", got)
	}
	r.pool.lose("p1")
	r.core.PodLost("p1", "the pod was deleted")
	r.store.failWith(nil)
	r.pool.add("p2", "A")
	r.dispatch()
	if got := r.next("p2"); got != "t1" {
		t.Fatalf("p2 got %q, want t1, which p1 was never handed", got)
	}
	if _, fails, _ := r.source.snapshot(); len(fails) != 0 {
		t.Fatalf("fails = %v, want none", fails)
	}
}
