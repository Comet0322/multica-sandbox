package dispatcher_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/dispatcher"
	"sandbox-manager/internal/lending"
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

// sharedPods uses the person-issue rule with the given pod capacity and hold
// timeout.
func sharedPods(capacity int, hold time.Duration) func(*dispatcher.PoolConfig) {
	return func(c *dispatcher.PoolConfig) {
		c.Placement = lending.RulePersonIssue
		c.Capacity = capacity
		c.HoldTimeout = hold
	}
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

// A bound pod does not claim by itself: an idle pod's reservation claims the
// task, which joins the binding of its key, and the bound pod picks it up.
func TestSameIssueTasksRunInParallelOnTheBoundPod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, 0))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()

	if got := a.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("first claim: %v", got)
	}
	if got := b.claimBatch(); len(got) != 0 {
		t.Fatalf("the idle pod took a task of the bound pod's key: %v", got)
	}
	if got := a.claimBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("bound pod should take the second task of its issue: %v", got)
	}
	got := leasePods(h)
	if got["t1"] != a.id || got["t2"] != a.id {
		t.Fatalf("both tasks should lease the first pod: %v", got)
	}
	if inv := h.inventory(); inv.Idle != 1 || inv.Leased != 1 {
		t.Fatalf("second pod should stay idle: %+v", inv)
	}
}

func TestSameIssueTasksOfDifferentPeopleNeverShareAPod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 3, sharedPods(5, 0))
	h.upstream.enqueueBy("t1", "issue-1", "", "u-alice")
	h.upstream.enqueueBy("t2", "issue-1", "", "u-bob")
	h.upstream.enqueueBy("t3", "issue-1", "", "U-ALICE ")
	h.upstream.enqueueBy("t4", "issue-1", "", "")
	h.upstream.enqueueBy("t5", "issue-1", "", "")
	a, b, c := h.newPod("pool-0"), h.newPod("pool-1"), h.newPod("pool-2")
	a.start()
	b.start()
	c.start()

	a.claimBatch()
	b.claimBatch()
	if got := c.claimBatch(); len(got) != 0 {
		t.Fatalf("alice's second task should join her binding, not the idle pod: %v", got)
	}
	a.claimBatch()
	c.claimBatch()
	got := leasePods(h)
	if got["t1"] != a.id || got["t3"] != a.id {
		t.Fatalf("one person's tasks should share a pod: %v", got)
	}
	if got["t2"] != b.id || got["t4"] != c.id {
		t.Fatalf("bob's task and the task without an initiator need pods of their own: %v", got)
	}
	// Every pod is bound, so the second task without an initiator stays queued.
	if q := h.upstream.queued(); q != 1 {
		t.Fatalf("want t5 queued upstream, %d queued", q)
	}
}

func TestChatTasksWithoutIssueShareByConversation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, 0))
	h.upstream.enqueueIn("t1", "", "chat-1")
	h.upstream.enqueueIn("t2", "", "chat-1")
	h.upstream.enqueueIn("t3", "", "chat-2")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()

	a.claimBatch()
	b.claimBatch()
	a.claimBatch()
	if got := b.claimBatch(); !slices.Equal(got, []string{"t3"}) {
		t.Fatalf("idle pod should get the other conversation's task: %v", got)
	}
	got := leasePods(h)
	if got["t1"] != a.id || got["t2"] != a.id || got["t3"] != b.id {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestTaskOfAFullBindingOpensANewBindingOnTheReservedPod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(2, 0))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	h.upstream.enqueueIn("t3", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	a.claimBatch()
	b.claimBatch()
	a.claimBatch()

	if got := b.claimBatch(); !slices.Equal(got, []string{"t3"}) {
		t.Fatalf("the reserved pod should take the overflow at once: %v", got)
	}
	got := leasePods(h)
	if got["t1"] != a.id || got["t2"] != a.id || got["t3"] != b.id {
		t.Fatalf("unexpected placement: %v", got)
	}
}

func TestTaskNoPodCanTakeStaysQueuedUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, sharedPods(3, time.Hour))
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

func TestSharedPodResetsOnlyAfterItsLastTaskEndsAndItsHoldRunsOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, 150*time.Millisecond))
	h.upstream.enqueueIn("t1", "issue-1", "")
	h.upstream.enqueueIn("t2", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	a.claimBatch()
	b.claimBatch()
	a.claimBatch()

	if status := a.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete t1: %d", status)
	}
	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("pod with a running task must not reset: %+v", inv)
	}
	if status := a.report("t2", "complete"); status != http.StatusOK {
		t.Fatalf("complete t2: %d", status)
	}
	if inv := h.inventory(); inv.Leased != 1 || inv.Resetting != 0 {
		t.Fatalf("pod should be in hold right after its last task: %+v", inv)
	}
	time.Sleep(300 * time.Millisecond)
	if inv := h.inventory(); inv.Resetting != 1 {
		t.Fatalf("pod should reset once its hold runs out: %+v", inv)
	}
	var reasons []string
	for _, r := range h.auditRecords() {
		if r.Action == dispatcher.AuditReset && r.PodID == a.id {
			reasons = append(reasons, r.Reason)
		}
	}
	if !slices.Contains(reasons, "reset started: hold expired") {
		t.Fatalf("the hold's expiry should be audited: %v", reasons)
	}
}

func TestInventoryShowsEachPodsBindingKeyAndHold(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2, sharedPods(3, time.Hour))
	h.upstream.enqueueIn("t1", "issue-1", "")
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	a.claimBatch()

	pods := map[string]dispatcher.PodInfo{}
	for _, pd := range h.inventory().Pods {
		pods[pd.ID] = pd
	}
	if pods[b.id].Binding != nil {
		t.Fatalf("an idle pod has no binding: %+v", pods[b.id].Binding)
	}
	got := pods[a.id].Binding
	wantKey := "person-issue:issue:issue-1/by:u-alice/in:" + harnessWorkspaceID
	if got == nil || got.AffinityKey != wantKey || got.Hold || got.UsedSlots != 1 || got.CapacitySlots != 3 || got.Tasks != 1 {
		t.Fatalf("running binding: %+v", got)
	}

	a.report("t1", "complete")
	got = nil
	for _, pd := range h.inventory().Pods {
		if pd.ID == a.id {
			got = pd.Binding
		}
	}
	if got == nil || !got.Hold || got.HoldUntil == nil || got.HoldRemainingSeconds < 3590 || got.HoldRemainingSeconds > 3600 || got.UsedSlots != 0 {
		t.Fatalf("held binding: %+v", got)
	}
}

func TestInvalidPlacementConfigIsRejected(t *testing.T) {
	t.Parallel()
	for name, opt := range map[string]func(*dispatcher.PoolConfig){
		"unknown rule":     func(c *dispatcher.PoolConfig) { c.Placement = "agent" },
		"old issue rule":   func(c *dispatcher.PoolConfig) { c.Placement = "issue" },
		"old chat rule":    func(c *dispatcher.PoolConfig) { c.Placement = "chat" },
		"negative size":    func(c *dispatcher.PoolConfig) { c.Capacity = -1 },
		"negative waiting": func(c *dispatcher.PoolConfig) { c.PlacementWait = -time.Second },
		"negative hold":    func(c *dispatcher.PoolConfig) { c.HoldTimeout = -time.Second },
	} {
		cfg := dispatcher.PoolConfig{
			MulticaURL: "http://127.0.0.1:1", Ticket: harnessPAT,
		}
		opt(&cfg)
		if _, err := dispatcher.NewPool(cfg, nil); err == nil || (strings.HasPrefix(name, "old") && !strings.Contains(err.Error(), "affinity rule")) {
			t.Errorf("%s: want an error naming the affinity rule, got %v", name, err)
		}
	}
}
