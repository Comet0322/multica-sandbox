package multica

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
)

// RuntimeMode is the mode the manager's runtimes are registered in: the
// manager appears as the bot account's own local daemon.
const RuntimeMode = "local"

// Identity is the machine identity the manager presents upstream in place of
// a pod daemon's own.
type Identity struct {
	WorkspaceID, DaemonID, Name string
	// Providers limits which of the daemon's agent CLIs become runtimes; empty
	// keeps every provider.
	Providers []string
	// Source names what was registered, for the error when nothing is left.
	Source string
	// Mode is the runtime_mode registered upstream: "cloud" when empty.
	Mode string
	// LegacyIDs are daemon_ids the same runtimes were registered under
	// before; the server merges their rows into this identity's.
	LegacyIDs []string
}

// RewriteRegistration turns a daemon's captured registration into the
// upstream one: one machine identity, no custom profiles.
func RewriteRegistration(captured json.RawMessage, id Identity) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(captured, &req); err != nil {
		return nil, fmt.Errorf("decode captured registration: %w", err)
	}
	runtimes, _ := req["runtimes"].([]any)
	var kept []any
	for _, item := range runtimes {
		rt, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := rt["type"].(string)
		if profile, _ := rt["profile_id"].(string); profile != "" {
			continue
		}
		if len(id.Providers) > 0 && !slices.Contains(id.Providers, typ) {
			continue
		}
		rt["runtime_mode"] = cmp.Or(id.Mode, "cloud")
		rt["name"] = id.Name
		if len(id.Providers) != 1 {
			rt["name"] = id.Name + " (" + typ + ")"
		}
		kept = append(kept, rt)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%s has none of the configured providers %v", id.Source, id.Providers)
	}
	body := map[string]any{
		"workspace_id": id.WorkspaceID,
		"daemon_id":    id.DaemonID,
		"device_name":  id.Name,
		"cli_version":  req["cli_version"],
		"runtimes":     kept,
	}
	if len(id.LegacyIDs) > 0 {
		body["legacy_daemon_ids"] = id.LegacyIDs
	}
	return body, nil
}

// RuntimeRef is a registered runtime as the server reports it.
type RuntimeRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// RuntimesOf lists the runtimes of a stored registration answer, nil when it
// has none or cannot be read.
func RuntimesOf(registration json.RawMessage) []RuntimeRef {
	var resp struct {
		Runtimes []RuntimeRef `json:"runtimes"`
	}
	if json.Unmarshal(registration, &resp) != nil || len(resp.Runtimes) == 0 {
		return nil
	}
	return resp.Runtimes
}

// RuntimeIDs lists the runtime IDs of a stored registration answer.
func RuntimeIDs(registration json.RawMessage) []string {
	runtimes := RuntimesOf(registration)
	ids := make([]string, 0, len(runtimes))
	for _, rt := range runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

// CoversProviders reports whether the stored registration has a runtime for
// every provider the registration body would register.
func CoversProviders(stored json.RawMessage, body map[string]any) bool {
	var have struct {
		Runtimes []struct {
			Provider string `json:"provider"`
		} `json:"runtimes"`
	}
	if json.Unmarshal(stored, &have) != nil {
		return true
	}
	known := map[string]bool{}
	for _, rt := range have.Runtimes {
		known[rt.Provider] = true
	}
	want, _ := body["runtimes"].([]any)
	for _, item := range want {
		rt, _ := item.(map[string]any)
		if typ, _ := rt["type"].(string); !known[typ] {
			return false
		}
	}
	return true
}

// AnswerForPod is the stored registration answer as a pod's daemon gets it:
// only the runtimes of the agent CLIs it offers. False when the answer
// cannot be read.
func AnswerForPod(registration json.RawMessage, offered map[string]bool) (map[string]json.RawMessage, bool) {
	var resp map[string]json.RawMessage
	var runtimes []map[string]any
	if json.Unmarshal(registration, &resp) != nil || json.Unmarshal(resp["runtimes"], &runtimes) != nil {
		return nil, false
	}
	kept := runtimes[:0]
	for _, rt := range runtimes {
		if provider, _ := rt["provider"].(string); offered[provider] {
			kept = append(kept, rt)
		}
	}
	resp["runtimes"], _ = json.Marshal(kept)
	return resp, true
}
