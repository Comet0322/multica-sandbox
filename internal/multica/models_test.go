package multica_test

import (
	"encoding/json"
	"testing"

	"sandbox-manager/internal/multica"
)

func TestParseModels(t *testing.T) {
	got, err := multica.ParseModels(" claude = claude-sonnet-4-6, claude-opus-4-7 ; codex=gpt-5.2;")
	if err != nil {
		t.Fatal(err)
	}
	var claude struct {
		Status string `json:"status"`
		Models []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
		} `json:"models"`
	}
	if err := json.Unmarshal(got["claude"], &claude); err != nil {
		t.Fatal(err)
	}
	if claude.Status != "completed" || len(claude.Models) != 2 || claude.Models[0].ID != "claude-sonnet-4-6" || !claude.Models[0].Default || claude.Models[1].Default {
		t.Fatalf("claude report = %s", got["claude"])
	}
	if len(got) != 2 || got["codex"] == nil {
		t.Fatalf("providers = %v, want claude and codex", got)
	}
	if !multica.UsableModelReport(got["codex"]) {
		t.Fatalf("a parsed report must be one the catalog keeps")
	}
	if none, err := multica.ParseModels(""); err != nil || none != nil {
		t.Fatalf("empty spec = %v, %v; want none", none, err)
	}
	for _, bad := range []string{"claude", "=a", "claude=", "claude=a;claude=b"} {
		if _, err := multica.ParseModels(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
