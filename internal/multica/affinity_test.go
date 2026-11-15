package multica_test

import (
	"encoding/json"
	"testing"

	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/policy"
)

func TestATasksKeyIsItsIssueElseItsChatSession(t *testing.T) {
	by := `"attribution":{"originator":{"id":"u-alice"}}`
	for _, c := range []struct{ raw, want string }{
		{`{"id":"t1","agent_id":"ag-1","issue_id":"is-1","chat_session_id":"ch-1",` + by + `}`, "issue:is-1"},
		{`{"id":"t2","agent_id":"ag-1","chat_session_id":"ch-1",` + by + `}`, "chat:ch-1"},
		{`{"id":"t3","agent_id":"ag-1",` + by + `}`, ""},
	} {
		claimed, err := multica.ParseClaimed(json.RawMessage(c.raw))
		if err != nil {
			t.Fatalf("ParseClaimed: %v", err)
		}
		got := claimed.PolicyTask("ws-a", policy.KindClean)
		if want := (policy.Task{ID: claimed.ID, Tenant: "ws-a", Kind: policy.KindClean, Key: c.want}); got != want {
			t.Errorf("%s: %+v, want %+v", c.raw, got, want)
		}
	}
}

func TestTheOldRuleNamesStillLoad(t *testing.T) {
	for _, rule := range []multica.AffinityRule{multica.RuleTask, multica.RulePersonIssue, multica.RulePersonAgent, multica.RulePerson, multica.RuleWorkspace} {
		if err := multica.CheckAffinityRule(rule); err != nil {
			t.Errorf("CheckAffinityRule(%s): %v", rule, err)
		}
	}
	if err := multica.CheckAffinityRule("pod"); err == nil {
		t.Error("an unknown rule is accepted")
	}
}

func TestAClaimWithoutAnIDIsRefused(t *testing.T) {
	if _, err := multica.ParseClaimed(json.RawMessage(`{"agent_id":"ag-1"}`)); err == nil {
		t.Fatal("a task without an ID was accepted")
	}
}
