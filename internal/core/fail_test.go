package core_test

import (
	"errors"
	"slices"
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
	want := "A t1 the pod running this task was lost: the pod was deleted"
	if _, fails, _ := r.source.snapshot(); len(fails) != 3 || slices.ContainsFunc(fails, func(f string) bool { return f != want }) {
		t.Fatalf("fails = %q, want the same report three times", fails)
	}
}
