package multica

import "sandbox-manager/internal/policy"

// Key is the key a task's pod is held for: its issue, else its chat session,
// empty for neither.
func (t ClaimedTask) Key() string {
	switch {
	case t.IssueID != "":
		return "issue:" + t.IssueID
	case t.ChatSessionID != "":
		return "chat:" + t.ChatSessionID
	}
	return ""
}

// PolicyTask is the claimed task as the policy knows it, in the workspace
// the claim was made in, for the runtime kind it was claimed for.
func (t ClaimedTask) PolicyTask(workspaceID string, kind policy.Kind) policy.Task {
	return policy.Task{ID: t.ID, Tenant: workspaceID, Kind: kind, Key: t.Key()}
}
