package lending_test

import "sandbox-manager/internal/lending"

// The policy knows only lending.RuleTask; these rules stand in for the ones a
// caller registers, sharing by the "issue", "chat" and "agent" labels.
const (
	rulePersonIssue lending.AffinityRule = "person-issue"
	rulePersonAgent lending.AffinityRule = "person-agent"
	rulePerson      lending.AffinityRule = "person"
)

func testRules() lending.Registry {
	return lending.Registry{
		rulePersonIssue: func(t lending.Task) string {
			switch {
			case t.Labels["issue"] != "":
				return "issue:" + t.Labels["issue"]
			case t.Labels["chat"] != "":
				return "chat:" + t.Labels["chat"]
			}
			return ""
		},
		rulePersonAgent: func(t lending.Task) string { return t.Labels["agent"] },
		rulePerson:      func(lending.Task) string { return "all" },
	}
}
