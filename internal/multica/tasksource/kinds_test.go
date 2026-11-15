package tasksource_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/multica/tasksource"
	"sandbox-manager/internal/policy"
)

// kindsMultica is a Multica that registers one runtime per daemon_id,
// "rt-<daemon_id>", and records what it is asked.
type kindsMultica struct {
	mu         sync.Mutex
	registered []map[string]any
	claims     []map[string]any
	heartbeats []string
}

func (m *kindsMultica) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.URL.Path == "/api/me":
		writeJSON(w, map[string]any{"id": "bot"})
	case r.URL.Path == "/api/workspaces" || r.URL.Path == "/api/daemon/workspaces":
		writeJSON(w, []map[string]any{{"id": wsA, "name": "A", "slug": "acme"}})
	case r.URL.Path == "/api/runtimes":
		writeJSON(w, []map[string]any{})
	case r.URL.Path == "/api/daemon/register":
		m.registered = append(m.registered, body)
		writeJSON(w, map[string]any{"runtimes": []map[string]any{{"id": "rt-" + body["daemon_id"].(string), "provider": "claude"}}})
	case strings.HasSuffix(r.URL.Path, "/recover-orphans"):
		writeJSON(w, map[string]any{})
	case r.URL.Path == "/api/daemon/heartbeat":
		m.heartbeats = append(m.heartbeats, body["runtime_id"].(string))
		writeJSON(w, map[string]any{"status": "ok"})
	case r.URL.Path == "/api/daemon/tasks/claim":
		m.claims = append(m.claims, body)
		writeJSON(w, map[string]any{"tasks": []map[string]any{{"id": "t1", "workspace_id": wsA, "agent_id": "ag", "issue_id": "is-1"}}})
	default:
		http.NotFound(w, r)
	}
}

func newKindsSource(t *testing.T, sets []tasksource.RuntimeSet) (*tasksource.Source, *kindsMultica) {
	t.Helper()
	up := &kindsMultica{}
	srv := httptest.NewServer(http.HandlerFunc(up.handle))
	t.Cleanup(srv.Close)
	s := tasksource.New(tasksource.Config{
		Client: multica.NewClient(srv.URL, "mul_pat", "mgr", nil), Name: "Pool", RuntimeVisibility: "private",
		Runtimes: sets,
	})
	s.Connect(&fakeCore{busy: map[string]bool{}}, &fakePods{})
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"workspace_id":"` + wsA + `","runtimes":[{"type":"claude","name":"claude"}]}`)
	if _, _, perr := s.Register(context.Background(), "pod-1", multica.Registration{Raw: raw, WorkspaceID: wsA, Offered: map[string]bool{"claude": true}}); perr != nil {
		t.Fatal(perr.Msg)
	}
	return s, up
}

var (
	poolSet  = tasksource.RuntimeSet{Kind: policy.KindPool, Name: "Pool", DeviceName: "pool-device"}
	cleanSet = tasksource.RuntimeSet{Kind: policy.KindClean, Name: "Clean Pool", DeviceName: "clean-device"}
)

func TestEachRuntimeKindRegistersUnderItsOwnDaemonID(t *testing.T) {
	for _, c := range []struct {
		name string
		sets []tasksource.RuntimeSet
		want []string // daemon_id name device_name of each registration
	}{
		{"default", nil, []string{"mgr Claude (Pool) Pool"}},
		{"pool", []tasksource.RuntimeSet{poolSet}, []string{"mgr Claude (Pool) pool-device"}},
		{"clean", []tasksource.RuntimeSet{cleanSet}, []string{"mgr-clean Claude (Clean Pool) clean-device"}},
		{"pool,clean", []tasksource.RuntimeSet{poolSet, cleanSet}, []string{"mgr Claude (Pool) pool-device", "mgr-clean Claude (Clean Pool) clean-device"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, up := newKindsSource(t, c.sets)
			up.mu.Lock()
			var got []string
			for _, body := range up.registered {
				rt := body["runtimes"].([]any)[0].(map[string]any)
				got = append(got, body["daemon_id"].(string)+" "+rt["name"].(string)+" "+body["device_name"].(string))
			}
			up.mu.Unlock()
			if !slices.Equal(got, c.want) {
				t.Fatalf("registered %q, want %q", got, c.want)
			}
			s.HeartbeatOnce(context.Background())
			up.mu.Lock()
			defer up.mu.Unlock()
			var beats []string
			for _, w := range c.want {
				beats = append(beats, "rt-"+strings.Fields(w)[0])
			}
			if !slices.Equal(up.heartbeats, beats) {
				t.Fatalf("heartbeats %v, want exactly %v", up.heartbeats, beats)
			}
		})
	}
}

func TestAPodGetsTheRuntimesOfEveryKind(t *testing.T) {
	up := &kindsMultica{}
	srv := httptest.NewServer(http.HandlerFunc(up.handle))
	t.Cleanup(srv.Close)
	s := tasksource.New(tasksource.Config{
		Client: multica.NewClient(srv.URL, "mul_pat", "mgr", nil), Name: "Pool", RuntimeVisibility: "private",
		Runtimes: []tasksource.RuntimeSet{poolSet, cleanSet},
	})
	s.Connect(&fakeCore{busy: map[string]bool{}}, &fakePods{})
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"workspace_id":"` + wsA + `","runtimes":[{"type":"claude","name":"claude"}]}`)
	resp, _, perr := s.Register(context.Background(), "pod-1", multica.Registration{Raw: raw, WorkspaceID: wsA, Offered: map[string]bool{"claude": true}})
	if perr != nil {
		t.Fatal(perr.Msg)
	}
	var runtimes []struct{ ID string }
	if err := json.Unmarshal(resp["runtimes"], &runtimes); err != nil {
		t.Fatal(err)
	}
	if len(runtimes) != 2 || runtimes[0].ID != "rt-mgr" || runtimes[1].ID != "rt-mgr-clean" {
		t.Fatalf("the pod's daemon must host both kinds' runtimes to run their tasks: %+v", runtimes)
	}
	if !s.RuntimeServed("rt-mgr-clean") {
		t.Fatal("the clean runtime is not served")
	}
}

func TestATaskIsOfTheKindItWasClaimedFor(t *testing.T) {
	s, up := newKindsSource(t, []tasksource.RuntimeSet{poolSet, cleanSet})
	for _, c := range []struct {
		kind               policy.Kind
		daemonID, runtimes string
	}{
		{policy.KindClean, "mgr-clean", "rt-mgr-clean"},
		{policy.KindPool, "mgr", "rt-mgr"},
	} {
		task, err := s.Claim(context.Background(), wsA, c.kind)
		if err != nil || task == nil {
			t.Fatalf("claim %s: %v %v", c.kind, task, err)
		}
		if want := (policy.Task{ID: "t1", Tenant: wsA, Kind: c.kind, Key: "issue:is-1"}); task.Task != want {
			t.Fatalf("claimed %+v, want %+v", task.Task, want)
		}
		up.mu.Lock()
		last := up.claims[len(up.claims)-1]
		up.mu.Unlock()
		if last["daemon_id"] != c.daemonID || len(last["runtime_ids"].([]any)) != 1 || last["runtime_ids"].([]any)[0] != c.runtimes {
			t.Fatalf("%s claim body %v", c.kind, last)
		}
	}
}
