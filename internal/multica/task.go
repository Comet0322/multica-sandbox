package multica

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ClaimedTask is the part of a claimed task the manager reads: which task it
// is, where it comes from and who started it. Raw is the task as Multica
// handed it out, which the pod's daemon gets unchanged.
type ClaimedTask struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	RuntimeID      string `json:"runtime_id"`
	WorkspaceID    string `json:"workspace_id"`
	IssueID        string `json:"issue_id"`
	ChatSessionID  string `json:"chat_session_id"`
	AutopilotRunID string `json:"autopilot_run_id"`
	InitiatorEmail string `json:"initiator_email"`
	Attribution    struct {
		Originator *struct {
			ID string `json:"id"`
		} `json:"originator"`
	} `json:"attribution"`

	Raw json.RawMessage `json:"-"`
}

// ParseClaimed reads a task a claim returned. A task without an ID cannot be
// placed or reported on; the server reclaims it once it is not started.
func ParseClaimed(raw json.RawMessage) (ClaimedTask, error) {
	var t ClaimedTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return ClaimedTask{}, err
	}
	if t.ID == "" {
		return ClaimedTask{}, errors.New("claimed task has no ID")
	}
	t.Raw = raw
	return t, nil
}

// Person is the user ID of the human who authorized the task
// (attribution.originator.id). Empty when no human did, as for autopilot
// runs.
func (t ClaimedTask) Person() string {
	if o := t.Attribution.Originator; o != nil {
		return strings.ToLower(strings.TrimSpace(o.ID))
	}
	return ""
}

// Email is the email of the human the task runs on behalf of, for the audit
// trail. Empty means the task has no human initiator (schedules,
// agent-triggered tasks).
func (t ClaimedTask) Email() string {
	return strings.ToLower(strings.TrimSpace(t.InitiatorEmail))
}

// PendingTask is a task on a runtime's pending list.
type PendingTask struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"created_at"`
}

// Created is when the task was created, zero when the server's time cannot
// be read.
func (t PendingTask) Created() time.Time {
	created, _ := time.Parse(time.RFC3339Nano, t.CreatedAt)
	return created
}

// TerminalTaskStatus lists the task states the stock daemon treats as over.
func TerminalTaskStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// StartAction reports whether a pod's task report says the daemon started the
// task.
func StartAction(action string) bool { return action == "start" }

// TerminalAction reports whether a pod's task report ends the task: a result,
// or the acknowledgement that it stopped after a cancellation (the stock daemon
// sends only cancel-ack, never complete or fail, for a task cancelled or ended
// by the server while it ran).
func TerminalAction(action string) bool {
	return action == "complete" || action == "fail" || action == "cancel-ack"
}

// TaskTokenPath matches daemon endpoints the server authorizes with a
// task-scoped token carried in the claim (RemoteMCPDaemonToken).
func TaskTokenPath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/api/daemon/"), "/")
	if len(parts) < 3 || parts[0] != "tasks" {
		return false
	}
	return parts[2] == "plugin-hooks" || (len(parts) == 5 && parts[4] == "credential")
}
