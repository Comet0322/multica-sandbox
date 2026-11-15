package manager_test

import (
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// enqueueIssue adds a task of alice's to the workspace under a chosen issue.
func (m *multiMultica) enqueueIssue(ws *fakeWorkspace, id, issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws.queue = append(ws.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": ws.runtimeID,
		"workspace_id": ws.id, "issue_id": issueID, "initiator_email": "alice@example.com",
		"attribution": originated("u-alice"),
	})
}

// A pod never serves another workspace: a held pod of A is not given B's
// task of the same issue ID, which takes another pod.
func TestAPodNeverServesAnotherWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newBotHarness(t, up, twoWorkspaces(nil, nil), manager.Config{UpstreamBackoff: time.Minute, HoldTimeout: time.Hour})
	up.enqueueIssue(a, "ta1", "issue-1")
	p1, p2 := h.newPod("pool-0"), h.newPod("pool-1")
	var ids []string
	for _, p := range []*simPod{p1, p2} {
		ids = p.startAll()
	}
	if got := p1.claimFrom(ids); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("p1 should take A's task: %v", got)
	}
	p1.report("ta1", "complete")

	up.enqueueIssue(b, "tb1", "issue-1")
	if got := p1.claimFrom(ids); len(got) != 0 {
		t.Fatalf("A's held pod took B's task: %v", got)
	}
	if got := p2.claimFrom(ids); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("p2 should take B's task: %v", got)
	}
}
