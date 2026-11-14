package manager_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// daemonIDsSent lists the daemon_id of every registration and claim the fake
// server received.
func daemonIDsSent(up *syncMultica) (registered, claimed []string) {
	for _, r := range up.requestsTo("/api/daemon/register") {
		id, _ := r.Body["daemon_id"].(string)
		registered = append(registered, id)
	}
	for _, r := range up.requestsTo("/api/daemon/tasks/claim") {
		id, _ := r.Body["daemon_id"].(string)
		claimed = append(claimed, id)
	}
	return registered, claimed
}

// serveTwoWorkspaces syncs a fresh manager against up, registers one pod in
// both workspaces and has it claim from each, as a pod daemon would.
func serveTwoWorkspaces(t *testing.T, up *syncMultica, mutate ...func(*manager.Config)) *simPod {
	t.Helper()
	h := newSyncHarness(t, up, mutate...)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("pool-0")
	pod.startAll()
	h.dispatch() // one claim per workspace
	return pod
}

func singleValue(t *testing.T, what string, ids []string) string {
	t.Helper()
	if len(ids) == 0 {
		t.Fatalf("no %s reached the server", what)
	}
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("%s daemon_ids differ: %v", what, ids)
		}
	}
	return ids[0]
}

func TestOneDaemonIdentityInEveryWorkspace(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	serveTwoWorkspaces(t, up)

	registered, claimed := daemonIDsSent(up)
	if len(registered) != 2 || len(claimed) != 2 {
		t.Fatalf("want a registration and a claim per workspace, got %v and %v", registered, claimed)
	}
	id := singleValue(t, "registration and claim", append(registered, claimed...))
	if strings.HasPrefix(id, "pod-") {
		t.Fatalf("the server received the pod's daemon_id %q", id)
	}
}

func TestDerivedDaemonIdentitySurvivesARestart(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	serveTwoWorkspaces(t, up)
	// A second manager on the same server and account is the restarted one.
	serveTwoWorkspaces(t, up)

	registered, _ := daemonIDsSent(up)
	if len(registered) != 4 {
		t.Fatalf("want two registrations per run, got %v", registered)
	}
	singleValue(t, "first run and restart", registered)
}

func TestDerivedDaemonIdentityDependsOnTheAccount(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	serveTwoWorkspaces(t, up)
	up.mu.Lock()
	up.accountID = "11111111-2222-4333-8444-000000000002"
	up.mu.Unlock()
	serveTwoWorkspaces(t, up)

	registered, _ := daemonIDsSent(up)
	if len(registered) != 4 {
		t.Fatalf("want two registrations per account, got %v", registered)
	}
	if singleValue(t, "first account", registered[:2]) == singleValue(t, "second account", registered[2:]) {
		t.Fatal("two bot accounts on one server share a daemon identity")
	}
}

func TestDaemonIdentityOverride(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	serveTwoWorkspaces(t, up, func(c *manager.Config) { c.DaemonID = "company-sandbox-prod" })

	registered, claimed := daemonIDsSent(up)
	if id := singleValue(t, "registration and claim", append(registered, claimed...)); id != "company-sandbox-prod" {
		t.Fatalf("daemon_id %q, want the configured one", id)
	}
	if up.pathCount("GET /api/me") != 0 {
		t.Fatal("the account was looked up although the identity is configured")
	}
}

// The runtime rows registered under the per-workspace identity the manager
// used before are merged into the new one, so agents stay bound to them.
func TestRegistrationMergesThePerWorkspaceIdentity(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	serveTwoWorkspaces(t, up)

	legacy := map[string]string{}
	for _, r := range up.requestsTo("/api/daemon/register") {
		ws, _ := r.Body["workspace_id"].(string)
		ids, _ := r.Body["legacy_daemon_ids"].([]any)
		if len(ids) != 1 {
			t.Fatalf("registration for %s: legacy_daemon_ids %v, want the old per-workspace id", ws, r.Body["legacy_daemon_ids"])
		}
		legacy[ws], _ = ids[0].(string)
	}
	// Known values of the former derivation, sha256(URL NUL workspace ID); the
	// URL is the fake's, so only their shape and distinctness are fixed here.
	if !strings.HasPrefix(legacy[wsA], "sandbox-broker-") || !strings.HasPrefix(legacy[wsB], "sandbox-broker-") || legacy[wsA] == legacy[wsB] {
		t.Fatalf("legacy ids %v", legacy)
	}
}

func TestPodsAreInvisibleUpstream(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	h := newSyncHarness(t, up)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, second := h.newPod("pool-0"), h.newPod("pool-1")
	// Every pod of a workspace gets the workspace's one runtime list.
	if a, b := first.startAll(), second.startAll(); len(a) != 2 || !slices.Equal(a, b) {
		t.Fatalf("pods got runtime lists %v and %v", a, b)
	}
	for _, ws := range []string{wsA, wsB} {
		if a, b := first.registerFor(ws), second.registerFor(ws); len(a) != 1 || !slices.Equal(a, b) {
			t.Fatalf("%s: pods got runtime lists %v and %v", ws, a, b)
		}
		second.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "rt-" + ws[:8]})
	}
	h.dispatch() // one claim per workspace
	if n := len(up.requestsTo("/api/daemon/tasks/claim")); n != 2 {
		t.Fatalf("claims upstream: %d, want one per workspace", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(up.requestsTo("/api/daemon/heartbeat")) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(up.requestsTo("/api/daemon/heartbeat")); n < 2 {
		t.Fatalf("heartbeats upstream: %d, want one per workspace", n)
	}

	registered, claimed := daemonIDsSent(up)
	manager := singleValue(t, "registration and claim", append(registered, claimed...))
	for _, r := range up.requestsTo("/api/daemon/register") {
		if name, _ := r.Body["device_name"].(string); name != "Pool" {
			t.Fatalf("device_name %q, want the configured name", name)
		}
	}
	for _, r := range up.allRequests() {
		for _, pod := range []string{"pool-0", "pool-1", first.id, second.id, first.token, second.token} {
			if strings.Contains(r.Raw, pod) {
				t.Fatalf("%s %s carries the pod's %q: %s", r.Method, r.Path, pod, r.Raw)
			}
			for k, vs := range r.Header {
				for _, v := range vs {
					if strings.Contains(v, pod) {
						t.Fatalf("%s %s header %s carries the pod's %q", r.Method, r.Path, k, pod)
					}
				}
			}
		}
	}
	if strings.Contains(manager, "pool-") {
		t.Fatalf("daemon_id %q names a pod", manager)
	}
}

func TestRuntimeNameIsTheConfiguredName(t *testing.T) {
	t.Parallel()
	up := newSyncMultica(t, wsA, wsB)
	serveTwoWorkspaces(t, up, func(c *manager.Config) {
		c.Name = "Company Sandbox"
		c.Providers = []string{"claude"}
	})
	for _, r := range up.requestsTo("/api/daemon/register") {
		runtimes, _ := r.Body["runtimes"].([]any)
		if len(runtimes) == 0 {
			t.Fatal("registration without runtimes")
		}
		for _, item := range runtimes {
			rt, _ := item.(map[string]any)
			if name, _ := rt["name"].(string); name != "Company Sandbox" {
				t.Fatalf("runtime name %q, want the configured name", name)
			}
		}
	}
}
