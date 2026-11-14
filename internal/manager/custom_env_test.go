package manager_test

import (
	"maps"
	"testing"
)

// The manager handles no git or LLM credentials (ADR 0002): an agent's custom
// environment is how it reaches an LLM and a git host, so the pod must receive
// it exactly as the server sent it.
func TestAgentCustomEnvReachesThePodUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	customEnv := map[string]string{
		"ANTHROPIC_BASE_URL":   "https://llm.example.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-agent-own",
		"GITLAB_TOKEN":         "glpat-agent-own",
	}
	h.upstream.enqueueTask("t1", "alice@example.com", customEnv)
	pod := h.newPod("pool-0")
	pod.start()

	task := pod.claimTask()
	if task == nil || task["id"] != "t1" {
		t.Fatalf("want task t1, got %v", task)
	}
	if got := envOf(t, task); !maps.Equal(got, customEnv) {
		t.Fatalf("custom env changed on the way to the pod:\nwant %v\ngot  %v", customEnv, got)
	}
}
