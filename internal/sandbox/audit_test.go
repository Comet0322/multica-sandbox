package sandbox_test

import (
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

// reasons lists the reset records of the pod, as decision: reason.
func (h *harness) resetRecords(id sandbox.PodID) []sandbox.AuditRecord {
	var out []sandbox.AuditRecord
	for _, r := range h.rec.records() {
		if r.PodID == string(id) && r.Action == sandbox.AuditReset {
			out = append(out, r)
		}
	}
	return out
}

func TestTheServiceAuditsTheResetsItStartsItself(t *testing.T) {
	h := newHarness(t)
	pod := h.register("pool-0")
	pod.completeReset()

	recs := h.resetRecords(pod.id)
	if len(recs) != 2 {
		t.Fatalf("want reset started and completed, got %+v", recs)
	}
	started, done := recs[0], recs[1]
	if started.Decision != sandbox.AuditAllow || started.Reason != "reset started: pod registered; it is reset before it is first lent" ||
		started.PodName != "pool-0" || started.TaskID != "" || started.Time.IsZero() {
		t.Fatalf("unexpected start record: %+v", started)
	}
	if done.Decision != sandbox.AuditAllow || done.Reason != "reset completed" {
		t.Fatalf("unexpected completion record: %+v", done)
	}
}

// The service records the start of the reset a Delete causes, and how it
// ended, both naming the caller's cause.
func TestAHandedBackPodsResetIsAuditedWithTheCallersCause(t *testing.T) {
	label := sandbox.AuditLabel{Reason: "task ended", TaskID: "t1", WorkspaceID: "ws-1", Initiator: "alice@example.com"}
	t.Run("completed", func(t *testing.T) {
		h := newHarness(t)
		pod := h.registerClean("pool-0")
		if err := h.svc.Delete(t.Context(), h.mustCreate(), label); err != nil {
			t.Fatal(err)
		}
		before := len(h.resetRecords(pod.id)) // the start is covered below
		cmd := pod.poll()
		pod.report(cmd.ID, "/home/pod/x", "/home/pod/y")

		recs := h.resetRecords(pod.id)[before:]
		want := sandbox.AuditRecord{Action: sandbox.AuditReset, Decision: sandbox.AuditAllow,
			Reason: "reset completed; 2 paths could not be deleted", TaskID: "t1", WorkspaceID: "ws-1",
			Initiator: "alice@example.com", PodID: string(pod.id), PodName: "pool-0"}
		if len(recs) != 1 || stripTime(recs[0]) != want {
			t.Fatalf("records = %+v, want only %+v", recs, want)
		}
	})
	t.Run("timed out", func(t *testing.T) {
		h := newHarness(t, shortReset)
		pod := h.registerClean("pool-0")
		if err := h.svc.Delete(t.Context(), h.mustCreate(), label); err != nil {
			t.Fatal(err)
		}
		before := len(h.resetRecords(pod.id)) // the start is covered below
		h.advance(3500 * time.Millisecond)
		h.svc.Inventory()

		recs := h.resetRecords(pod.id)[before:]
		want := sandbox.AuditRecord{Action: sandbox.AuditReset, Decision: sandbox.AuditDeny,
			Reason: "reset timed out: the supervisor did not report it done in time", TaskID: "t1", WorkspaceID: "ws-1",
			Initiator: "alice@example.com", PodID: string(pod.id), PodName: "pool-0"}
		if len(recs) != 1 || stripTime(recs[0]) != want {
			t.Fatalf("records = %+v, want only %+v", recs, want)
		}
	})
}

func TestDeleteAuditsTheResetStartOnlyWhenItStartsOne(t *testing.T) {
	label := sandbox.AuditLabel{Reason: "task ended", TaskID: "t1", WorkspaceID: "ws-1", Initiator: "alice@example.com"}
	h := newHarness(t)
	pod := h.registerClean("pool-0")
	id := h.mustCreate()
	before := len(h.resetRecords(pod.id))
	if err := h.svc.Delete(t.Context(), id, label); err != nil {
		t.Fatal(err)
	}
	recs := h.resetRecords(pod.id)[before:]
	want := sandbox.AuditRecord{Action: sandbox.AuditReset, Decision: sandbox.AuditAllow,
		Reason: "reset started: task ended", TaskID: "t1", WorkspaceID: "ws-1",
		Initiator: "alice@example.com", PodID: string(pod.id), PodName: "pool-0"}
	if len(recs) != 1 || stripTime(recs[0]) != want {
		t.Fatalf("records = %+v, want only %+v", recs, want)
	}

	// The pod is resetting now, and an unknown pod is no pod: both are no-ops
	// and leave no record.
	if err := h.svc.Delete(t.Context(), id, label); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Delete(t.Context(), "pod-unknown", label); err != nil {
		t.Fatal(err)
	}
	if got := h.resetRecords(pod.id)[before:]; len(got) != 1 {
		t.Fatalf("a no-op Delete left a record: %+v", got)
	}
	if got := h.rec.records(); len(got) != 0 && got[len(got)-1].PodID == "" {
		t.Fatalf("a Delete of an unknown pod left a record: %+v", got[len(got)-1])
	}
}

func TestALostPodIsAudited(t *testing.T) {
	h := newHarness(t, shortHeartbeat)
	pod := h.registerClean("pool-0")
	before := len(h.resetRecords(pod.id))
	h.advance(3500 * time.Millisecond)
	h.svc.Inventory()

	recs := h.resetRecords(pod.id)[before:]
	if len(recs) != 1 || recs[0].Decision != sandbox.AuditDeny || recs[0].Reason != "pod lost: heartbeat timed out" {
		t.Fatalf("records = %+v", recs)
	}
}

func stripTime(r sandbox.AuditRecord) sandbox.AuditRecord {
	r.Time = time.Time{}
	return r
}
