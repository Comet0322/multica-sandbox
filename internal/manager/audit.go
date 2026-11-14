package manager

import (
	"encoding/json"
	"io"
	"sandbox-manager/internal/multica"
	"sync"
	"time"
)

// Audit actions and decisions. Every lending decision of the manager is
// one JSON line, so a task ID leads to the person and pod behind it.
const (
	AuditLease   = "lease"
	AuditRelease = "release"
	AuditReset   = "reset"

	AuditAllow = "allow"
	AuditDeny  = "deny"
)

// AuditRecord is one line of the audit log. It never carries credentials: the
// claimed task body is not copied into it.
type AuditRecord struct {
	Time        time.Time `json:"time"`
	Action      string    `json:"action"`
	Decision    string    `json:"decision"`
	Reason      string    `json:"reason"`
	TaskID      string    `json:"task_id"`
	WorkspaceID string    `json:"workspace_id"`
	Initiator   string    `json:"initiator,omitempty"`
	PodID       string    `json:"pod_id,omitempty"`
	PodName     string    `json:"pod_name,omitempty"`
}

// auditLog writes JSON Lines. Writes are serialized, and a failing sink is
// reported through the pool's logger instead of blocking task handling.
type auditLog struct {
	mu    sync.Mutex
	w     io.Writer
	err   func(error)
	clock *clock
}

func newAuditLog(w io.Writer, clk *clock, onError func(error)) *auditLog {
	if w == nil {
		return nil
	}
	return &auditLog{w: w, err: onError, clock: clk}
}

func (a *auditLog) write(rec AuditRecord) {
	if a == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err == nil {
		a.mu.Lock()
		_, err = a.w.Write(append(line, '\n'))
		a.mu.Unlock()
	}
	if err != nil && a.err != nil {
		a.err(err)
	}
}

// record stamps the record with the time and writes it.
func (a *auditLog) record(rec AuditRecord) {
	if a == nil {
		return
	}
	rec.Time = a.clock.now().UTC()
	a.write(rec)
}

// audit records one decision about a task.
func (p *Manager) audit(rec AuditRecord) {
	p.auditLog.record(rec)
}

// auditLease records an event on an existing lease. Callers hold p.mu or own
// the lease exclusively.
func (p *Manager) auditLease(action string, l *lease, reason string) {
	if l == nil {
		return
	}
	p.audit(AuditRecord{
		Action: action, Decision: AuditAllow, Reason: reason,
		TaskID: l.TaskID, WorkspaceID: l.WorkspaceID, Initiator: l.Initiator, PodID: l.PodID, PodName: l.PodName,
	})
}

// auditDenied records a task the manager refused to lend a pod to.
func (p *Manager) auditDenied(ws *workspace, task multica.ClaimedTask, reason string) {
	p.audit(AuditRecord{
		Action: AuditLease, Decision: AuditDeny, Reason: reason,
		TaskID: task.ID, WorkspaceID: ws.cfg.WorkspaceID, Initiator: task.Email(),
	})
}
