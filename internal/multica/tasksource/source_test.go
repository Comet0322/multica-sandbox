package tasksource_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/multica/tasksource"
	"sandbox-manager/internal/policy"
)

const (
	wsA = "aaaaaaaa-0000-4000-8000-000000000001"
	wsB = "bbbbbbbb-0000-4000-8000-000000000002"
)

// fakeCore records what the source tells the core.
type fakeCore struct {
	mu      sync.Mutex
	tenants []core.Tenant
}

func (c *fakeCore) SetTenants(_ context.Context, list []core.Tenant) core.TenantChanges {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tenants = list
	return core.TenantChanges{}
}

// fakePods records what the source tells the pod side.
type fakePods struct {
	mu       sync.Mutex
	notified []bool
	models   []string
}

func (p *fakePods) NotifyWorkspacesChanged(now bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notified = append(p.notified, now)
}

func (p *fakePods) NoteModelRequest(runtimeID, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models = append(p.models, runtimeID+" "+id)
}

func (p *fakePods) HasLivePods() bool { return true }

// newSource starts a fake Multica serving two workspaces with handle as the
// fallback, and a source on it.
func newSource(t *testing.T, overrides string, handle func(w http.ResponseWriter, r *http.Request) bool) (*tasksource.Source, *fakeCore, *fakePods) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle != nil && handle(w, r) {
			return
		}
		switch r.URL.Path {
		case "/api/me":
			writeJSON(w, map[string]any{"id": "bot"})
		case "/api/daemon/workspaces", "/api/workspaces":
			writeJSON(w, []map[string]any{{"id": wsA, "name": "A", "slug": "acme"}, {"id": wsB, "name": "B", "slug": "beta"}})
		case "/api/runtimes":
			writeJSON(w, []map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := tasksource.Config{
		Client: multica.NewClient(srv.URL, "mul_pat", "", nil), RuntimeVisibility: "private",
		Runtimes: []tasksource.RuntimeSet{{Kind: policy.KindPool, Machine: multica.Machine{Name: "Pool"}}},
	}
	if overrides != "" {
		cfg.ReadOverrides = func() ([]byte, error) { return []byte(overrides), nil }
	}
	s := tasksource.New(cfg)
	c, p := &fakeCore{}, &fakePods{}
	s.Connect(c, p)
	return s, c, p
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestSyncTellsTheCoreEachWorkspaceWithItsShareSettings(t *testing.T) {
	s, c, pods := newSource(t, "acme: {guarantee: 2, weight: 3, quota: 4}\n", nil)
	res, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 2 {
		t.Fatalf("added %v", res.Added)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got := map[string]policy.TenantSettings{}
	for _, tn := range c.tenants {
		got[tn.ID] = tn.Settings
	}
	if a := got[wsA]; a.Guarantee != 2 || a.Weight != 3 || a.Quota != 4 {
		t.Fatalf("acme settings %+v", a)
	}
	if b, ok := got[wsB]; !ok || b.Guarantee != 0 || b.Weight != 0 {
		t.Fatalf("beta settings %+v (listed %v)", b, ok)
	}
	pods.mu.Lock()
	defer pods.mu.Unlock()
	if len(pods.notified) != 1 || !pods.notified[0] {
		t.Fatalf("the pods must look at the new workspaces at once: %v", pods.notified)
	}
	if s.NotReady() != "" {
		t.Fatalf("not ready after a good listing: %s", s.NotReady())
	}
}

func TestAWorkspaceIsClaimableOnlyOnceAPodRegisteredIt(t *testing.T) {
	s, _, _ := newSource(t, "", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/daemon/register":
			writeJSON(w, map[string]any{"runtimes": []map[string]any{{"id": "rt-a", "provider": "claude"}}})
		case "/api/daemon/tasks/claim":
			writeJSON(w, map[string]any{"tasks": []map[string]any{{"id": "t1", "workspace_id": strings.ToUpper(wsA), "agent_id": "ag", "issue_id": "is", "initiator_email": "Alice@Example.com"}}})
		default:
			return false
		}
		return true
	})
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.Claimable(); len(got) != 0 {
		t.Fatalf("claimable before any registration: %v", got)
	}
	raw := json.RawMessage(`{"workspace_id":"` + wsA + `","runtimes":[{"type":"claude","name":"claude"}]}`)
	resp, id, perr := s.Register(context.Background(), "pod-1", multica.Registration{Raw: raw, WorkspaceID: wsA, Offered: map[string]bool{"claude": true}})
	if perr != nil || id != wsA || len(resp["runtimes"]) == 0 {
		t.Fatalf("register: %v %q %v", perr, id, resp)
	}
	if got := s.Claimable(); len(got) != 1 || got[0] != wsA {
		t.Fatalf("claimable after registration: %v", got)
	}
	task, err := s.Claim(context.Background(), wsA, policy.KindPool)
	if err != nil || task == nil {
		t.Fatalf("claim: %v %v", task, err)
	}
	if task.ID != "t1" || task.Tenant != wsA || task.Kind != policy.KindPool || task.Email != "alice@example.com" || !strings.Contains(string(task.Payload), `"t1"`) {
		t.Fatalf("claimed task %+v", task)
	}
}

func TestAFailureThatDoesNotGetThroughIsRetried(t *testing.T) {
	var mu sync.Mutex
	down := true
	fails := 0
	s, _, _ := newSource(t, "", func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/fail" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		fails++
		if down {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return true
		}
		writeJSON(w, map[string]any{})
		return true
	})
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(context.Background(), wsA, "t1", "pod lost"); err == nil {
		t.Fatal("a report Multica could not take returned no error")
	}
	mu.Lock()
	down = false
	mu.Unlock()
	if err := s.Fail(context.Background(), wsA, "t1", "pod lost"); err != nil {
		t.Fatalf("a report Multica took: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fails != 2 {
		t.Fatalf("fail calls %d, want the first and the retry", fails)
	}
}

func TestTheSourceHeartbeatsItsRuntimesAndPassesModelRequestsOn(t *testing.T) {
	s, _, pods := newSource(t, "", func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/daemon/register":
			writeJSON(w, map[string]any{"runtimes": []map[string]any{{"id": "rt-a", "provider": "claude"}}})
		case "/api/daemon/heartbeat":
			writeJSON(w, map[string]any{"status": "ok", "pending_model_list": map[string]string{"id": "ml-1"}})
		default:
			return false
		}
		return true
	})
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"workspace_id":"` + wsA + `","runtimes":[{"type":"claude","name":"claude"}]}`)
	if _, _, perr := s.Register(context.Background(), "pod-1", multica.Registration{Raw: raw, WorkspaceID: wsA, Offered: map[string]bool{"claude": true}}); perr != nil {
		t.Fatal(perr.Msg)
	}
	s.HeartbeatOnce(context.Background())
	pods.mu.Lock()
	defer pods.mu.Unlock()
	if len(pods.models) != 1 || pods.models[0] != "rt-a ml-1" {
		t.Fatalf("model requests passed on: %v", pods.models)
	}
}
