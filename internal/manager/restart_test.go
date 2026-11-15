package manager_test

import (
	"net/http"
	"slices"
	"testing"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/sandbox"
)

// The manager keeps what each pod serves on the pod, so a manager that
// restarts carries on: the running task runs on and reports as before, the
// held pod takes its key's next task, and the idle and warm pods are kept.
func TestARestartedManagerCarriesOnWithItsPods(t *testing.T) {
	w := newKubeWorld(t)
	h1 := w.manager(4, 4)
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

	h2 := w.manager(4, 4)
	for _, p := range pods {
		p.h = h2
	}
	// The running pod's daemon registered with the manager that stopped; its
	// next heartbeat makes it register again.
	if code := running.heartbeat(); code != http.StatusNotFound {
		t.Fatalf("heartbeat of a pod the restarted manager knows no registration of: %d, want 404", code)
	}
	for _, p := range pods {
		p.startRaw()
	}
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
	// The warm pod is still there to take the next new key.
	w.upstream.enqueueIssue("t6", "issue-6")
	if got := h2.runTask("t6", pods); got != warm && got != running {
		t.Fatalf("t6 went to %s", got.id)
	}
	if n := len(h2.kubePods()); n != 4 {
		t.Fatalf("pods after the restart = %d, want the 4 kept", n)
	}
}

// A task Multica shows dispatched to the manager that no pod has is failed
// once a restarted manager finds its workspace busy with restored work.
func TestARestartedManagerFailsDispatchedTasksItLost(t *testing.T) {
	w := newKubeWorld(t)
	daemonID := func(c *manager.Config) { c.DaemonID = "test-daemon" }
	h1 := w.manager(2, 2, daemonID)
	pods := h1.startedPods(2)
	w.upstream.enqueueIssue("t1", "issue-1")
	running := h1.runTask("t1", pods)
	eventually(t, "the running pod's record", func() bool {
		return h1.kubePod(running.id).Annotations[sandbox.TaskAnnotation] == "t1"
	})
	h1.stop()

	// t9 was claimed by the manager that stopped, and never reached a pod.
	w.upstream.intercept(func(rw http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/runtimes":
			writeJSON(rw, []map[string]any{{"id": "runtime-a", "visibility": "public", "daemon_id": "test-daemon"}})
			return true
		case "/api/daemon/runtimes/runtime-a/tasks/pending":
			writeJSON(rw, []map[string]any{
				{"id": "t1", "runtime_id": "runtime-a", "status": "dispatched"},
				{"id": "t9", "runtime_id": "runtime-a", "status": "dispatched"},
				{"id": "t8", "runtime_id": "runtime-a", "status": "queued"},
			})
			return true
		}
		return false
	})
	recovered := w.upstream.callCount("POST /api/daemon/runtimes/runtime-a/recover-orphans")
	w.manager(2, 2, daemonID)
	eventually(t, "t9 failed", func() bool { _, ok := w.upstream.failureOf("t9"); return ok })
	if _, ok := w.upstream.failureOf("t1"); ok {
		t.Fatal("the restored running task was failed")
	}
	if _, ok := w.upstream.failureOf("t8"); ok {
		t.Fatal("a queued task was failed")
	}
	if n := w.upstream.callCount("POST /api/daemon/runtimes/runtime-a/recover-orphans") - recovered; n != 0 {
		t.Fatalf("recover-orphans called %d times on a runtime with a live task", n)
	}
}
