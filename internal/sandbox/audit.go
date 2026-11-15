package sandbox

import "time"

// Audit actions and decisions, as the manager's audit log spells them.
const (
	AuditReset  = "reset"
	AuditDelete = "delete"

	AuditAllow = "allow"
	AuditDeny  = "deny"
)

// AuditRecord is one line of the audit log, in the manager's shape.
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

// AuditWriter takes the service's audit records. The caller injects it.
type AuditWriter interface {
	Record(AuditRecord)
}

// AuditLabel names what caused a reset, for the audit trail only: the
// service copies it into the records of the reset's start, completion or
// timeout.
type AuditLabel struct {
	Reason      string
	TaskID      string
	WorkspaceID string
	Initiator   string
}

// auditReset records a reset event of the pod, tied to the cause the caller
// named when it handed the pod back. Callers hold p.mu.
func (p *GitOpsService) auditReset(pd *pod, decision, reason string) {
	if p.cfg.Audit == nil {
		return
	}
	l := pd.resetLabel
	p.cfg.Audit.Record(AuditRecord{
		Time: p.now().UTC(), Action: AuditReset, Decision: decision, Reason: reason,
		TaskID: l.TaskID, WorkspaceID: l.WorkspaceID, Initiator: l.Initiator,
		PodID: string(pd.id), PodName: pd.label,
	})
}
