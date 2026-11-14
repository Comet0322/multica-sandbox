package dispatcher_test

import (
	"context"
	"net/http"
	"testing"
)

// callWithToken sends one request the way an agent's multica CLI does: to the
// manager, with the task's own token instead of the daemon's placeholder.
func (p *simPod) callWithToken(ticket, method, path string) int {
	p.h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, p.h.podAPI.URL+path, nil)
	if err != nil {
		p.h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ticket)
	resp, err := p.client.Do(req)
	if err != nil {
		p.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (f *fakeMultica) agentCallsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.agentCalls...)
}

// The agent's multica CLI talks to the same server address as the daemon, which
// in a pod is the manager, and authenticates with a task token minted by the
// server. The manager must relay that traffic unchanged (it never sees the
// workspace PAT on it), or no agent can read its issue or post a result.
func TestAgentCLITrafficWithATaskTokenReachesTheServerUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()

	if status := pod.callWithToken("mat_task_token", http.MethodGet, "/api/issues/abc"); status != http.StatusOK {
		t.Fatalf("agent request: status %d", status)
	}
	got := h.upstream.agentCallsSeen()
	if len(got) != 1 || got[0] != "GET /api/issues/abc Bearer mat_task_token" {
		t.Fatalf("server saw %q, want the request with the agent's own token", got)
	}
}

// The task token is the credential, so the manager relays it without
// identifying the pod that sent it: a caller holding no pod token is relayed
// too.
func TestAgentCLITrafficIsRelayedWithoutIdentifyingThePod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	anyone := &simPod{h: h, name: "anyone", client: &http.Client{}}

	if status := anyone.callWithToken("mat_task_token", http.MethodGet, "/api/issues/xyz"); status != http.StatusOK {
		t.Fatalf("agent request: status %d", status)
	}
	got := h.upstream.agentCallsSeen()
	if len(got) != 1 || got[0] != "GET /api/issues/xyz Bearer mat_task_token" {
		t.Fatalf("server saw %q, want the request with the agent's own token", got)
	}
}

// Agent paths carry the agent's task token, never a pod token: a pod token
// there proves nothing and is refused, and so is a call with neither.
func TestAgentPathsWithoutATaskTokenAreRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()

	if status := pod.callWithToken(pod.token, http.MethodGet, "/api/issues/abc"); status != http.StatusForbidden {
		t.Errorf("pod token on an agent path: status %d, want 403", status)
	}
	if status := pod.callWithToken("mul_some_pat", http.MethodGet, "/api/issues/abc"); status != http.StatusUnauthorized {
		t.Errorf("another credential on an agent path: status %d, want 401", status)
	}
	if got := h.upstream.agentCallsSeen(); len(got) != 0 {
		t.Errorf("refused requests reached the server: %q", got)
	}
}
