package manager_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// originated is a task payload's attribution naming the person who authorized
// it, as Multica v0.6.4 sends it; nil when no person did.
func originated(person string) map[string]any {
	if person == "" {
		return nil
	}
	return map[string]any{"originator": map[string]any{"id": person}}
}

// enqueueIn adds a task of alice's with the given issue and chat session IDs.
func (f *fakeMultica) enqueueIn(id, issueID, chatID string) {
	f.enqueueBy(id, issueID, chatID, "u-alice")
}

// enqueueBy adds a task authorized by the person with the given user ID.
func (f *fakeMultica) enqueueBy(id, issueID, chatID, person string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task := map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": issueID, "chat_session_id": chatID,
	}
	if person != "" {
		task["initiator_email"] = person + "@example.com"
		task["attribution"] = originated(person)
	}
	f.queue = append(f.queue, task)
}

// holding holds a pod for its task's key for hold once the task ended.
func holding(hold time.Duration) func(*manager.Config) {
	return func(c *manager.Config) { c.HoldTimeout = hold }
}

func leasePods(h *harness) map[string]string {
	out := map[string]string{}
	for _, l := range h.leases() {
		if l.EndedAt == nil {
			out[l.TaskID] = l.PodID
		}
	}
	return out
}

// A handoff: the next task of an issue runs on the pod held for it, though
// the claim was made against the other pod, which stays as it was.
func TestTheNextTaskOfAnIssueRunsOnItsHeldPod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, holding(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	if got := a.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("first claim: %v", got)
	}
	a.report("t1", "complete")
	h.upstream.enqueueIn("t2", "issue-1", "")
	if got := b.claimBatch(); len(got) != 0 {
		t.Fatalf("the other pod took a task of the held pod's key: %v", got)
	}
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the held pod should take the next task of its issue: %v", got)
	}
}

// One task at a time: a second task of the issue, while the first runs,
// takes another pod.
func TestTwoTasksOfOneIssueNeverShareAPod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, holding(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	a.claimBatch()
	b.claimBatch()
	got := leasePods(h)
	if got["t1"] == "" || got["t2"] == "" || got["t1"] == got["t2"] {
		t.Fatalf("each task needs a pod of its own: %v", got)
	}
}

func TestInventoryShowsEachPodsKeyAndHold(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, holding(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	a.claimBatch()

	pods := map[string]manager.PodInfo{}
	for _, pd := range h.inventory().Pods {
		pods[pd.ID] = pd
	}
	if got := pods[b.id]; got.Phase != manager.PodWarm || got.Key != "" || got.Kind != "" || got.WorkspaceID != "" {
		t.Fatalf("a pod that served no one: %+v", got)
	}
	got := pods[a.id]
	if got.Phase != manager.PodRunning || got.Key != "issue:issue-1" || got.TaskID != "t1" || got.Kind != "pool" || got.WorkspaceID != harnessWorkspaceID || got.HoldUntil != nil {
		t.Fatalf("running pod: %+v", got)
	}

	a.report("t1", "complete")
	for _, pd := range h.inventory().Pods {
		if pd.ID == a.id {
			got = pd
		}
	}
	if got.Phase != manager.PodHeld || got.Key != "issue:issue-1" || got.TaskID != "" || got.HoldUntil == nil || got.HoldRemainingSeconds < 3590 || got.HoldRemainingSeconds > 3600 {
		t.Fatalf("held pod: %+v", got)
	}
}

// With the budget full and its only pod running, a task stays queued in
// Multica; once that pod is held, it is deleted for the task, and the pod
// created in its place takes it.
func TestTaskNoPodCanTakeStaysQueuedUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()

	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("the running pod was handed another task: %v", got)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("the issue-2 task should stay queued in Multica")
	}
	if status := a.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete t1: %d", status)
	}
	h.dispatch() // the held pod is deleted for the budget
	b := h.newPod("pool-1")
	b.start()
	if got := b.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("the new pod should get the queued task: %v", got)
	}
}
