package multica

import (
	"fmt"
	"strings"

	"sandbox-manager/internal/policy"
)

// AffinityRule names one of the affinity rules of the GitOps pool. Since ADR
// 0009 no rule decides anything: a pod runs one task at a time and is held
// for its task's key (see PolicyTask). The names are still read, and
// checked, so an existing configuration loads; issue 07 removes them.
type AffinityRule string

// The affinity rule names a configuration may still give.
const (
	RuleTask        AffinityRule = "task"
	RulePersonIssue AffinityRule = "person-issue"
	RulePersonAgent AffinityRule = "person-agent"
	RulePerson      AffinityRule = "person"
	RuleWorkspace   AffinityRule = "workspace"
)

// DefaultAffinityRule is the rule a configuration that names none had.
const DefaultAffinityRule = RulePersonIssue

// affinityRules lists the rule names in the order errors name them.
var affinityRules = []AffinityRule{RuleTask, RulePersonIssue, RulePersonAgent, RulePerson, RuleWorkspace}

// CheckAffinityRule refuses a rule name that names no affinity rule. Empty is
// the default.
func CheckAffinityRule(rule AffinityRule) error {
	if rule == "" {
		return nil
	}
	for _, known := range affinityRules {
		if rule == known {
			return nil
		}
	}
	names := make([]string, len(affinityRules))
	for i, known := range affinityRules {
		names[i] = string(known)
	}
	return fmt.Errorf("unknown affinity rule %q (want one of %s)", rule, strings.Join(names, ", "))
}

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
