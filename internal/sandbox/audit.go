package sandbox

import "time"

// Audit actions and decisions, as the manager's audit log spells them.
const (
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

// AuditLabel names what caused a deletion, for the audit trail only.
type AuditLabel struct {
	Reason      string
	TaskID      string
	WorkspaceID string
	Initiator   string
}
