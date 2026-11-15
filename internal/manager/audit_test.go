package manager_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/policy"
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
// no issue or chat session, so no key: its clean pod is deleted as soon as it
// ends, whatever the hold timeout.
func TestTaskWithoutInitiatorOrKeyRunsAndIsNeverHeld(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour), clean)
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
	eventually(t, "the pod deleted", func() bool {
		for _, p := range h.inventory().Pods {
			if p.ID == pod.id {
				return false
			}
		}
		return true
	})
	if inv := h.inventory(); inv.Held != 0 {
		t.Fatalf("its pod should be deleted at once, not held: %+v", inv)
	}
}

func TestAuditTracesLeaseReleaseAndDeleteByTaskID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, clean)
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
			r.PodID != pod.id || r.PodName != pod.id {
			t.Fatalf("incomplete record: %+v", r)
		}
		seq = append(seq, r.Action)
	}
	if strings.Join(seq, ",") != "lease,release,delete" {
		t.Fatalf("want lease, release, then the deletion it causes for t1, got %v", seq)
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

// clean claims for the clean kind, whose pod is deleted after a task with no
// hold.
func clean(c *manager.Config) { c.Runtimes = []policy.Kind{policy.KindClean} }
