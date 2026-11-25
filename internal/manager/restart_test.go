package manager_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/sandbox"
)

// The manager keeps what each pod serves on the pod, so a manager that
// restarts carries on: the running task runs on and reports as before, the
// held pod takes its key's next task, and the idle pod is kept. The warm pod
// started and carries no record: a task may have been placed on it before
// its record was written, so it is deleted rather than given another key.
func TestARestartedManagerCarriesOnWithItsPods(t *testing.T) {
	w := newKubeWorld(t)
	hold := func(c *manager.Config) { c.HoldTimeout = 10 * time.Minute }
	h1 := w.manager(4, 4, hold)
	pods := h1.startedPods(4)

	w.upstream.enqueueIssue("t1", "issue-1")
	running := h1.runTask("t1", pods)
	w.upstream.enqueueIssue("t2", "issue-2")
	held := h1.runTask("t2", pods)
	if held.report("t2", "complete") != http.StatusOK {
		t.Fatal("t2's completion refused")
	}
	w.upstream.enqueueNoInitiator("t3")
	idle := h1.runTask("t3", pods)
	if idle.report("t3", "complete") != http.StatusOK {
		t.Fatal("t3's completion refused")
	}
	var warm *simPod
	for _, p := range pods {
		if p != running && p != held && p != idle {
			warm = p
		}
	}
	phase := func(h *harness, p *simPod) string {
		return h.kubePod(p.id).Annotations[sandbox.PhaseAnnotation]
	}
	eventually(t, "the pods' records written", func() bool {
		return phase(h1, running) == "running" && phase(h1, held) == "held" && phase(h1, idle) == "idle"
	})
	if pd := h1.kubePod(running.id); pd.Annotations[sandbox.TaskAnnotation] != "t1" || pd.Labels[sandbox.WorkspaceLabel] != harnessWorkspaceID {
		t.Fatalf("running pod's record: labels %v, annotations %v", pd.Labels, pd.Annotations)
	}
	h1.stop()

	h2 := w.manager(4, 4, hold)
	for _, p := range pods {
		p.h = h2
	}
	// The running pod's daemon registered with the manager that stopped; its
	// next heartbeat makes it register again.
	if code := running.heartbeat(); code != http.StatusNotFound {
		t.Fatalf("heartbeat of a pod the restarted manager knows no registration of: %d, want 404", code)
	}
	for _, p := range []*simPod{running, held, idle} {
		p.startRaw()
	}
	eventually(t, "the unrecorded warm pod deleted", func() bool {
		for _, pd := range h2.kubePods() {
			if pd.Name == warm.id {
				return false
			}
		}
		return true
	})
	if code := running.heartbeat(); code != http.StatusOK {
		t.Fatalf("heartbeat after registering again: %d", code)
	}
	if code := running.report("t1", "progress"); code != http.StatusOK {
		t.Fatalf("t1's progress report after the restart: %d", code)
	}
	if code := running.report("t1", "complete"); code != http.StatusOK {
		t.Fatalf("t1's completion after the restart: %d", code)
	}
	h2.wantEnded("t1", "pod reported complete")
	if _, failed := w.upstream.failureOf("t1"); failed {
		t.Fatal("the restart failed the running task")
	}
	if !slices.Contains(w.upstream.taskCallsSeen(), "t1 complete") {
		t.Fatalf("t1's completion was not relayed: %v", w.upstream.taskCallsSeen())
	}

	// The held pod takes its key's next task.
	w.upstream.enqueueIssue("t4", "issue-2")
	if got := h2.runTask("t4", pods); got != held {
		t.Fatalf("t4 of issue-2 went to %s, want the held pod %s", got.id, held.id)
	}
	// Another key takes the idle pod, not the warm one.
	w.upstream.enqueueIssue("t5", "issue-5")
	if got := h2.runTask("t5", pods); got != idle {
		t.Fatalf("t5 went to %s, want the idle pod %s", got.id, idle.id)
	}
	if n := len(h2.kubePods()); n > 4 {
		t.Fatalf("pods after the restart = %d, want at most the budget of 4", n)
	}
}
