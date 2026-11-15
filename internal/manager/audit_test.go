package manager_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

func (h *harness) auditRecords() []manager.AuditRecord {
	h.t.Helper()
	var out []manager.AuditRecord
	sc := bufio.NewScanner(strings.NewReader(h.audit.String()))
	for sc.Scan() {
		var rec manager.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			h.t.Fatalf("audit line is not standalone JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

// A task no person authorized (an autopilot run) is no longer refused. It has
// no issue or chat session, so no key: its clean pod is reset as soon as it
// ends, whatever the hold timeout.
func TestTaskWithoutInitiatorOrKeyRunsAndIsNeverHeld(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour))
	h.upstream.enqueueNoInitiator("t1")
	pod := h.newPod("pool-0")
	pod.start()

	if got := pod.claimBatch(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("want t1 leased, got %v", got)
	}
	if _, failed := h.upstream.failureOf("t1"); failed {
		t.Fatal("a task without an initiator must not be failed")
	}
	var lease *manager.AuditRecord
	for _, r := range h.auditRecords() {
		if r.Action == manager.AuditLease && r.TaskID == "t1" {
			lease = &r
		}
	}
	if lease == nil || lease.Decision != manager.AuditAllow || lease.PodID != pod.id || lease.Initiator != "" {
		t.Fatalf("want the lease audited with no initiator, got %+v", lease)
	}
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: %d", status)
	}
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("its pod should reset at once, not hold: %+v", inv)
	}
}

func TestAuditTracesLeaseAndReleaseByTaskID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	pod := h.newPod("pool-0")
	pod.start()
	pod.claimBatch()
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: %d", status)
	}

	var seq []string
	for _, r := range h.auditRecords() {
		if r.TaskID != "t1" {
			continue
		}
		if r.Time.IsZero() || r.Reason == "" || r.Decision != manager.AuditAllow ||
			r.WorkspaceID != harnessWorkspaceID || r.Initiator != "alice@example.com" ||
			r.PodID != pod.id || r.PodName != "pool-0" {
			t.Fatalf("incomplete record: %+v", r)
		}
		seq = append(seq, r.Action)
	}
	if strings.Join(seq, ",") != "lease,release,reset" {
		t.Fatalf("want lease, release, then the reset it causes for t1, got %v", seq)
	}
}

func TestAuditIsJSONLinesWithoutCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.upstream.enqueue("t1")
	h.upstream.enqueueNoInitiator("t2")
	pod := h.newPod("pool-0")
	pod.start()
	pod.claimBatch()
	pod.report("t1", "fail")
	pod.completeReset()
	pod.claimBatch()

	raw := h.audit.String()
	if len(h.auditRecords()) < 3 {
		t.Fatalf("expected several records, got %q", raw)
	}
	for _, sealed := range []string{harnessPAT, pod.token, "Bearer"} {
		if strings.Contains(raw, sealed) {
			t.Fatalf("audit log contains %q: %s", sealed, raw)
		}
	}
}
