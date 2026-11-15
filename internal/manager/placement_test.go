package manager_test

import (
	"net/http"
	"slices"
	"strings"
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

func TestAHeldPodResetsWhenItsHoldRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(150*time.Millisecond))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()
	if status := a.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete t1: %d", status)
	}
	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("pod should be held right after its task: %+v", inv)
	}
	time.Sleep(300 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("pod should reset once its hold runs out: %+v", inv)
	}
	var reasons []string
	for _, r := range h.auditRecords() {
		if r.Action == manager.AuditReset && r.PodID == a.id {
			reasons = append(reasons, r.Reason)
		}
	}
	if !slices.Contains(reasons, "reset started: hold expired") {
		t.Fatalf("the hold's expiry should be audited: %v", reasons)
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
	if pods[b.id].Binding != nil {
		t.Fatalf("a pod that served no one has no binding: %+v", pods[b.id].Binding)
	}
	got := pods[a.id]
	if got.Binding == nil || got.Binding.AffinityKey != "issue:issue-1" || got.Binding.Hold || got.Binding.Tasks != 1 || got.Phase != "running" || got.Kind != "clean" {
		t.Fatalf("running pod: %+v %+v", got, got.Binding)
	}

	a.report("t1", "complete")
	for _, pd := range h.inventory().Pods {
		if pd.ID == a.id {
			got = pd
		}
	}
	if b := got.Binding; b == nil || !b.Hold || b.HoldUntil == nil || b.HoldRemainingSeconds < 3590 || b.HoldRemainingSeconds > 3600 || b.Tasks != 0 || got.Phase != "held" {
		t.Fatalf("held pod: %+v %+v", got, b)
	}
}

func TestTaskNoPodCanTakeStaysQueuedUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, holding(time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-2", "")
	a := h.newPod("pool-0")
	a.start()
	a.claimBatch()

	if got := a.claimBatch(); len(got) != 0 {
		t.Fatalf("pod bound to issue-1 was handed an issue-2 task: %v", got)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("the issue-2 task should stay queued in Multica")
	}
	b := h.newPod("pool-1")
	b.start()
	if got := b.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("new idle pod should get the queued task: %v", got)
	}
}

func TestInvalidPlacementConfigIsRejected(t *testing.T) {
	t.Parallel()
	for name, opt := range map[string]func(*manager.Config){
		"unknown rule":     func(c *manager.Config) { c.AffinityRule = "agent" },
		"old issue rule":   func(c *manager.Config) { c.AffinityRule = "issue" },
		"old chat rule":    func(c *manager.Config) { c.AffinityRule = "chat" },
		"negative size":    func(c *manager.Config) { c.Capacity = -1 },
		"negative waiting": func(c *manager.Config) { c.PlacementWait = -time.Second },
		"negative hold":    func(c *manager.Config) { c.HoldTimeout = -time.Second },
	} {
		cfg := manager.Config{
			MulticaURL: "http://127.0.0.1:1", PAT: harnessPAT,
		}
		opt(&cfg)
		if _, err := manager.New(cfg, nil); err == nil || (strings.HasPrefix(name, "old") && !strings.Contains(err.Error(), "affinity rule")) {
			t.Errorf("%s: want an error naming the affinity rule, got %v", name, err)
		}
	}
}
