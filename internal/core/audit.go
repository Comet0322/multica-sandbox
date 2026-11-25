package core

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Audit actions and decisions. Every decision of the manager is
// one JSON line, so a task ID leads to the person and pod behind it.
const (
	AuditLease   = "lease"
	AuditRelease = "release"

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

// AuditLog writes JSON Lines. Writes are serialized, and a failing sink is
// reported through onError instead of blocking task handling. A nil
// *AuditLog writes nothing.
type AuditLog struct {
	mu    sync.Mutex
	w     io.Writer
	err   func(error)
	clock *Clock
}

// NewAuditLog returns the audit log writing to w, nil when w is nil.
func NewAuditLog(w io.Writer, clk *Clock, onError func(error)) *AuditLog {
	if w == nil {
		return nil
	}
	return &AuditLog{w: w, err: onError, clock: clk}
}

// Write writes a record as it is.
func (a *AuditLog) Write(rec AuditRecord) {
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

// Record stamps the record with the time and writes it.
func (a *AuditLog) Record(rec AuditRecord) {
	if a == nil {
		return
	}
	rec.Time = a.clock.Now().UTC()
	a.Write(rec)
}

// auditLease records an event on an existing lease. Callers hold c.mu or own
// the lease exclusively.
func (c *Core) auditLease(action string, l *lease, reason string) {
	if l == nil {
		return
	}
	c.cfg.Audit.Record(AuditRecord{
		Action: action, Decision: AuditAllow, Reason: reason,
		TaskID: l.ID, WorkspaceID: l.Tenant, Initiator: l.Email, PodID: l.PodID, PodName: l.PodName,
	})
}

// auditDenied records a task the manager refused to lend a pod to.
func (c *Core) auditDenied(tenant string, t *Task, reason string) {
	c.cfg.Audit.Record(AuditRecord{
		Action: AuditLease, Decision: AuditDeny, Reason: reason,
		TaskID: t.ID, WorkspaceID: tenant, Initiator: t.Email,
	})
}
