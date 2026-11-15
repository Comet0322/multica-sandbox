package manager_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The daemon lists the workspaces before it registers anywhere; that start-up
// flow must not be refused.
func TestStartUpFlowIsNotBlockedByTheListAuthorization(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	if status, _ := pod.call(http.MethodPost, "/api/tokens/current/renew", map[string]any{}); status != http.StatusOK {
		t.Fatalf("renew: status %d", status)
	}
	status, raw := pod.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), harnessWorkspaceID) {
		t.Fatalf("list: status %d %s", status, raw)
	}
	if status, raw := pod.register(); status != http.StatusOK {
		t.Fatalf("register: status %d %s", status, raw)
	}
}

func repoStatus(p *simPod, workspaceID string) int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodGet, "/api/daemon/workspaces/"+workspaceID+"/repos", nil)
	return status
}

// The repo list is read with the manager's PAT, so it goes only to a pod
// working for that workspace.
func TestRepoListIsServedOnlyToAPodWorkingForTheWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))

	stranger := h.newPod("stranger")
	for _, ws := range []string{wsA, wsB} {
		if status := repoStatus(stranger, ws); status != http.StatusForbidden {
			t.Fatalf("unregistered address read %s: status %d", ws, status)
		}
	}

	// A pod that registered for both workspaces and is idle may read both.
	idle := h.newPod("pool-0")
	ids := idle.startAll()
	for _, ws := range []string{wsA, wsB} {
		if status := repoStatus(idle, ws); status != http.StatusOK {
			t.Fatalf("idle pod, %s: status %d", ws, status)
		}
	}

	// Leased for A, it reads A and not B.
	up.enqueue(a, "ta1")
	if got := idle.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want ta1, got %v", got)
	}
	if status := repoStatus(idle, wsA); status != http.StatusOK {
		t.Fatalf("leased pod reading its own workspace: status %d", status)
	}
	if status := repoStatus(idle, wsB); status != http.StatusForbidden {
		t.Fatalf("leased pod reading another workspace: status %d", status)
	}

	// Once the lease ends its clean pod is deleted and reads nothing more.
	if status := idle.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	if status := repoStatus(idle, wsA); status != http.StatusUnauthorized {
		t.Fatalf("deleted pod: status %d", status)
	}
}

func TestRepoListOfAPodThatRegisteredForOneWorkspaceStopsThere(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-1")
	pod.registerFor(wsA)
	if status := repoStatus(pod, wsA); status != http.StatusOK {
		t.Fatalf("warm pod, workspace it registered: status %d", status)
	}
	if status := repoStatus(pod, wsB); status != http.StatusForbidden {
		t.Fatalf("warm pod, workspace it did not register: status %d", status)
	}
}
