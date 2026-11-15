package multica

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A model list request is the picker in the Multica UI asking which models the
// runtime's agent CLI can run. Multica hands it to the runtime in the answer to
// a heartbeat. The runtime is the manager's, but the CLIs are in the pods, whose
// image is the same everywhere, so the manager passes the request to one pod's
// daemon, which asks its CLI and reports to the manager, which relays the
// report upstream with the workspace's account. Since one image means one
// catalog, the task source keeps that report (ModelCatalog) and answers the
// next requests of every runtime with the same provider itself for a while.

const (
	// modelRequestRetry is how long a pod has to report before the request is
	// handed to a pod again.
	modelRequestRetry = 45 * time.Second
	// modelRequestExpiry is when an unanswered request is forgotten; Multica
	// has timed it out by then.
	modelRequestExpiry = 3 * time.Minute
)

// modelRequest is a model list request Multica made for a runtime.
type modelRequest struct {
	id     string
	seen   time.Time
	handed time.Time
	handTo string // ID of the pod it was handed to
}

// noteModelRequest remembers the request the upstream heartbeat brought.
func (a *DaemonAPI) NoteModelRequest(runtimeID, id string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if a.modelReqs == nil {
		a.modelReqs = map[string]*modelRequest{}
	}
	if cur := a.modelReqs[runtimeID]; cur != nil && cur.id == id {
		return
	}
	a.modelReqs[runtimeID] = &modelRequest{id: id, seen: a.now()}
}

// takeModelRequest returns the request id to put into the heartbeat answer for
// the pod, or "" when there is none for it.
func (a *DaemonAPI) takeModelRequest(runtimeID, podID string) string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	r := a.modelReqs[runtimeID]
	if r == nil {
		return ""
	}
	now := a.now()
	if now.Sub(r.seen) > modelRequestExpiry {
		delete(a.modelReqs, runtimeID)
		return ""
	}
	if r.handTo != "" && now.Sub(r.handed) < modelRequestRetry {
		return ""
	}
	r.handTo, r.handed = podID, now
	return r.id
}

// modelRequestHeldBy reports whether the request was handed to the pod.
func (a *DaemonAPI) modelRequestHeldBy(runtimeID, id, podID string) bool {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	r := a.modelReqs[runtimeID]
	return r != nil && r.id == id && r.handTo == podID
}

// finishModelRequest forgets the request once Multica has taken the report.
func (a *DaemonAPI) finishModelRequest(runtimeID, id string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if r := a.modelReqs[runtimeID]; r != nil && r.id == id {
		delete(a.modelReqs, runtimeID)
	}
}

// ModelCatalogTTL is how long a pod's model list report answers later
// requests for every runtime of the same provider. The pods run one image, so
// one pod's report holds for all of them; it is asked again after this, so a
// new image's CLI is picked up.
const ModelCatalogTTL = 10 * time.Minute

// ModelCatalog keeps the latest model list report per provider, to answer
// later requests without a pod. It is safe for concurrent use.
type ModelCatalog struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	entries map[string]catalogEntry
}

type catalogEntry struct {
	report json.RawMessage
	at     time.Time
	pinned bool
}

// NewModelCatalog returns an empty catalog; ttl <= 0 means ModelCatalogTTL.
func NewModelCatalog(now func() time.Time, ttl time.Duration) *ModelCatalog {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = ModelCatalogTTL
	}
	return &ModelCatalog{now: now, ttl: ttl, entries: map[string]catalogEntry{}}
}

// UsableModelReport reports whether a model list report is worth reusing: a
// completed report naming models, not a fallback the CLI substituted after
// discovery failed.
func UsableModelReport(report []byte) bool {
	var r struct {
		Status   string            `json:"status"`
		Models   []json.RawMessage `json:"models"`
		Fallback bool              `json:"fallback"`
	}
	return json.Unmarshal(report, &r) == nil && r.Status == "completed" && len(r.Models) > 0 && !r.Fallback
}

// Remember keeps a report for the provider, when it is usable.
func (c *ModelCatalog) Remember(provider string, report []byte) bool {
	if provider == "" || !UsableModelReport(report) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[provider] = catalogEntry{report: append(json.RawMessage(nil), report...), at: c.now()}
	return true
}

// Pin keeps a report for the provider that never expires; a fresher pod
// report still takes its place while it is fresh.
func (c *ModelCatalog) Pin(provider string, report json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries["pinned/"+provider] = catalogEntry{report: report, pinned: true}
}

// Report returns the report to answer the provider's request with: a fresh
// pod report, else a pinned one.
func (c *ModelCatalog) Report(provider string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[provider]; ok && c.now().Sub(e.at) < c.ttl {
		return e.report, true
	}
	if e, ok := c.entries["pinned/"+provider]; ok {
		return e.report, true
	}
	return nil, false
}

// StaticModelReport builds a completed report naming the model IDs.
func StaticModelReport(ids []string) json.RawMessage {
	type entry struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Default bool   `json:"default,omitempty"`
	}
	models := make([]entry, 0, len(ids))
	for i, id := range ids {
		models = append(models, entry{ID: id, Label: id, Default: i == 0})
	}
	raw, _ := json.Marshal(map[string]any{"status": "completed", "models": models, "supported": true})
	return raw
}

// ModelReportPath is the upstream path a model list report is posted to.
func ModelReportPath(runtimeID, requestID string) string {
	return "/api/daemon/runtimes/" + runtimeID + "/models/" + requestID + "/result"
}

// ParseModels reads the models the picker offers per agent CLI, as
// "claude=claude-sonnet-4-6,claude-opus-4-7;codex=gpt-5.2": providers apart by
// ";", each with its comma list of model IDs, the first one the default. It
// returns a report per provider (see StaticModelReport); empty is none.
func ParseModels(spec string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, part := range strings.Split(spec, ";") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		provider, list, ok := strings.Cut(part, "=")
		provider = strings.TrimSpace(provider)
		if !ok || provider == "" {
			return nil, fmt.Errorf("%q must be provider=model,model", part)
		}
		var ids []string
		for _, id := range strings.Split(list, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("%s names no model", provider)
		}
		if _, dup := out[provider]; dup {
			return nil, fmt.Errorf("%s is listed twice", provider)
		}
		out[provider] = StaticModelReport(ids)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
