package multica_test

import (
	"encoding/json"
	"testing"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/multica"
)

// keyOf claims the task under rule on a fresh pod and returns the affinity
// key of the binding it opened.
func keyOf(t *testing.T, rule lending.AffinityRule, raw string) string {
	t.Helper()
	claimed, err := multica.ParseClaimed(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseClaimed: %v", err)
	}
	p, err := lending.New(lending.Config{Capacity: lending.Resources{Slots: 1}, Rules: multica.AffinityRules(), Rule: rule})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.SetTenant("ws-a", lending.TenantSettings{})
	p.PodAcquired("p1", "ws-a")
	p.RequestClaim(lending.ClaimRequest{Pods: []string{"p1"}, Tenants: []string{"ws-a"}})
	task := claimed.LendingTask("ws-a")
	p.ClaimReturned(1, &task, false)
	b, ok := p.Binding("p1")
	if !ok {
		t.Fatal("no binding on p1")
	}
	return b.Key
}

func TestTheRulesShareByWhatTheTaskIsAbout(t *testing.T) {
	by := `"attribution":{"originator":{"id":" U-Alice "}}`
	full := `{"id":"t1","agent_id":"ag-1","issue_id":"is-1","chat_session_id":"ch-1",` + by + `}`
	chat := `{"id":"t2","agent_id":"ag-1","chat_session_id":"ch-1",` + by + `}`
	bare := `{"id":"t3","agent_id":"ag-1",` + by + `}`
	anon := `{"id":"t4","agent_id":"ag-1","issue_id":"is-1"}`
	for _, c := range []struct {
		rule lending.AffinityRule
		raw  string
		want string
	}{
		{multica.RuleTask, full, "task:t1"},
		{multica.RulePersonIssue, full, "person-issue:issue:is-1/by:u-alice/in:ws-a"},
		{multica.RulePersonIssue, chat, "person-issue:chat:ch-1/by:u-alice/in:ws-a"},
		{multica.RulePersonIssue, bare, "task:t3"},
		{multica.RulePersonAgent, full, "person-agent:ag-1/by:u-alice/in:ws-a"},
		{multica.RulePerson, full, "person:all/by:u-alice/in:ws-a"},
		{multica.RulePerson, anon, "task:t4"},
	} {
		if got := keyOf(t, c.rule, c.raw); got != c.want {
			t.Errorf("%s for %s: key %q, want %q", c.rule, c.raw, got, c.want)
		}
	}
}

func TestEveryNamedRuleIsRegistered(t *testing.T) {
	rules := multica.AffinityRules()
	for _, rule := range []lending.AffinityRule{multica.RulePersonIssue, multica.RulePersonAgent, multica.RulePerson} {
		if rules[rule] == nil {
			t.Errorf("rule %s is accepted but not registered", rule)
		}
		if err := multica.CheckAffinityRule(rule); err != nil {
			t.Errorf("CheckAffinityRule(%s): %v", rule, err)
		}
	}
	if err := multica.CheckAffinityRule(multica.RuleTask); err != nil {
		t.Errorf("CheckAffinityRule(task): %v", err)
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
