package core_test

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

// A failure report the source did not take is sent again on every
// reconciliation pass until the source takes it.
func TestAFailureReportIsSentUntilTheSourceTakesIt(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	r.source.failWith(errors.New("upstream unavailable"))
	r.pool.lose("p1")
	r.core.PodLost("p1", "the pod was deleted")
	want := "A t1 the pod running this task was lost: the pod was deleted"
	r.wantFails(want)
	if n := r.core.PendingFailReports(); n != 1 {
		t.Fatalf("pending reports = %d after a report that did not get through, want 1", n)
	}
	r.core.ReconcileOnce(t.Context())
	if n := r.core.PendingFailReports(); n != 1 {
		t.Fatalf("pending reports = %d while the source still fails, want 1", n)
	}
	r.source.failWith(nil)
	r.core.ReconcileOnce(t.Context())
	if n := r.core.PendingFailReports(); n != 0 {
		t.Fatalf("pending reports = %d once the source took it, want 0", n)
	}
	if _, fails, _ := r.source.snapshot(); len(fails) != 3 || slices.ContainsFunc(fails, func(f string) bool { return f != want }) {
		t.Fatalf("fails = %q, want the same report three times", fails)
	}
}

// A failure report is in flight once at a time: a reconciliation pass while
// the first attempt is under way does not send it again.
func TestAFailureReportIsNotSentTwiceAtOnce(t *testing.T) {
	r := newRig(t, core.Config{}, tenant("A", policy.TenantSettings{}))
	r.pool.add("p1", "A")
	r.source.add("A", "t1", "alice", 0)
	r.dispatch()
	r.next("p1")
	sending, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r.source.whenFail(func(string) {
		if calls.Add(1) == 1 {
			close(sending)
			<-release
		}
	})
	t.Cleanup(func() { close(release) })

	go func() {
		r.pool.lose("p1")
		r.core.PodLost("p1", "the pod was deleted")
	}()
	<-sending
	r.core.ReconcileOnce(t.Context())
	if _, fails, _ := r.source.snapshot(); len(fails) != 0 {
		t.Fatalf("fails = %q while the first report is under way, want none sent again", fails)
	}
}
