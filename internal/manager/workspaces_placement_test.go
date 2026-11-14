package manager_test

import (
	"slices"
	"testing"

	"sandbox-manager/internal/lending"
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

func TestWorkspacePlacementDrivesSharingAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	// A keeps the pool default, person-issue; B gives every task its own pod.
	h := newMultiHarness(t, up, twoWorkspaces(nil, func(c *manager.WorkspaceConfig) { c.AffinityRule = lending.RuleTask }))
	up.enqueueIssue(a, "ta1", "issue-1")
	up.enqueueIssue(a, "ta2", "issue-1")
	p1, p2, p3 := h.newPod("pool-0"), h.newPod("pool-1"), h.newPod("pool-2")
	var ids []string
	for _, p := range []*simPod{p1, p2, p3} {
		ids = p.startAll()
	}

	first := p1.claimFrom(ids)
	if got := p2.claimFrom(ids); len(got) != 0 {
		t.Fatalf("A's second task should join the first pod's binding: %v", got)
	}
	got := slices.Concat(first, p1.claimFrom(ids))
	if !slices.Equal(got, []string{"ta1", "ta2"}) {
		t.Fatalf("workspace A's issue should share the first pod: %v", got)
	}

	// B's tasks never share, even under one issue: each needs its own pod.
	up.enqueueIssue(b, "tb1", "issue-9")
	up.enqueueIssue(b, "tb2", "issue-9")
	if got := p2.claimFrom(ids); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("pod 2 should take one B task: %v", got)
	}
	if got := p2.claimFrom(ids); len(got) != 0 {
		t.Fatalf("pod 2 must not take a second B task: %v", got)
	}
	if got := p3.claimFrom(ids); !slices.Equal(got, []string{"tb2"}) {
		t.Fatalf("pod 3 should take the other B task: %v", got)
	}
}
