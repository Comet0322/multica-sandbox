package multica

import (
	"fmt"
	"strings"

	"sandbox-manager/internal/lending"
)

// The affinity rules a pool or workspace may name. The lending policy knows
// only lending.RuleTask; the others share a pod by what a Multica task is
// about, read from the labels LendingTask sets.
const (
	RuleTask = lending.RuleTask
	// RulePersonIssue shares a binding between one person's tasks on one
	// issue, or in one chat session for a task without an issue. It is the
	// default.
	RulePersonIssue lending.AffinityRule = "person-issue"
	// RulePersonAgent shares a binding between one person's tasks of one agent.
	RulePersonAgent lending.AffinityRule = "person-agent"
	// RulePerson shares a binding between all of one person's tasks.
	RulePerson lending.AffinityRule = "person"
	// RuleWorkspace shares a binding between every task of a workspace,
	// whoever it runs for, and keeps it: its pod stays bound to the
	// workspace, in hold when idle, until another workspace's task evicts it
	// beyond the workspace's guarantee (lending.RuleTenant).
	RuleWorkspace lending.AffinityRule = "workspace"
)

// LendingRule is the lending policy's name of a rule: RuleWorkspace is the
// policy's RuleTenant, the others keep their names.
func LendingRule(rule lending.AffinityRule) lending.AffinityRule {
	if rule == RuleWorkspace {
		return lending.RuleTenant
	}
	return rule
}

// DefaultAffinityRule is the rule a pool uses when none is configured.
const DefaultAffinityRule = RulePersonIssue

// The labels LendingTask sets, which the rules read.
const (
	labelIssue = "issue"
	labelChat  = "chat"
	labelAgent = "agent"
)

// AffinityRules returns the rules the manager registers with the lending
// policy.
func AffinityRules() lending.Registry {
	return lending.Registry{
		RulePersonIssue: func(t lending.Task) string {
			switch {
			case t.Labels[labelIssue] != "":
				return "issue:" + t.Labels[labelIssue]
			case t.Labels[labelChat] != "":
				return "chat:" + t.Labels[labelChat]
			}
			return ""
		},
		RulePersonAgent: func(t lending.Task) string { return t.Labels[labelAgent] },
		RulePerson:      func(lending.Task) string { return "all" },
	}
}

// affinityRules lists the rule names in the order errors name them.
var affinityRules = []lending.AffinityRule{RuleTask, RulePersonIssue, RulePersonAgent, RulePerson, RuleWorkspace}

// CheckAffinityRule refuses a rule name that names no affinity rule. Empty is
// the default.
func CheckAffinityRule(rule lending.AffinityRule) error {
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

// LendingTask is the claimed task as the lending policy knows it, in the
// workspace the claim was made in. A task without a known initiator runs too;
// the policy gives it a binding of its own.
func (t ClaimedTask) LendingTask(workspaceID string) lending.Task {
	labels := map[string]string{}
	for k, v := range map[string]string{labelIssue: t.IssueID, labelChat: t.ChatSessionID, labelAgent: t.AgentID} {
		if v != "" {
			labels[k] = v
		}
	}
	return lending.Task{ID: t.ID, Tenant: workspaceID, Initiator: t.Person(), Labels: labels}
}
