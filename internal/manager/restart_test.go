package manager_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/manager"
)

// The manager keeps its dispatch record in its store, so a manager that
// restarts carries on: the running task runs on and reports as before, the
// held pod takes its key's next task, and the idle pod is kept. The warm pod
// is not in the record and is deleted; a new one takes its place.
func TestARestartedManagerCarriesOnWithItsPods(t *testing.T) {
	w := newKubeWorld(t)
	hold := func(c *manager.Config) { c.HoldTimeout = 10 * time.Minute }
	h1 := w.manager(4, 4, hold)
	pods := h1.startedPods(4)

	w.upstream.enqueueIssue("t1", "issue-1")
	running := h1.runTask("t1", pods)
	if running.report("t1", "start") != http.StatusOK {
		t.Fatal("t1's start refused")
	}
	w.upstream.enqueueIssue("t2", "issue-2")
	held := h1.runTask("t2", pods)
	if held.report("t2", "start") != http.StatusOK || held.report("t2", "complete") != http.StatusOK {
		t.Fatal("t2's reports refused")
	}
	w.upstream.enqueueNoInitiator("t3")
	idle := h1.runTask("t3", pods)
	if idle.report("t3", "start") != http.StatusOK || idle.report("t3", "complete") != http.StatusOK {
		t.Fatal("t3's reports refused")
	}
	var warm *simPod
	for _, p := range pods {
		if p != running && p != held && p != idle {
			warm = p
		}
	}
	if st := w.store.task("t1").State; st != core.TaskRunning {
		t.Fatalf("t1 is %q in the record, want running", st)
	}
	h1.stop()

	h2 := w.manager(4, 4, hold)
	for _, p := range pods {
		p.h = h2
	}
	if h2.kubeHas(warm.id) {
		t.Fatalf("the warm pod %s, not in the record, was kept", warm.id)
	}
	// The running pod's daemon registered with the manager that stopped; its
	// next heartbeat makes it register again.
	if code := running.heartbeat(); code != http.StatusNotFound {
		t.Fatalf("heartbeat of a pod the restarted manager knows no registration of: %d, want 404", code)
	}
	for _, p := range []*simPod{running, held, idle} {
		p.startRaw()
	}
	if code := running.heartbeat(); code != http.StatusOK {
		t.Fatalf("heartbeat after registering again: %d", code)
	}
	if got := running.pollBatch(); len(got) != 0 {
		t.Fatalf("the running task's pod was handed %v", got)
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
	// Another key takes the idle pod, not a warm one.
	w.upstream.enqueueIssue("t5", "issue-5")
	if got := h2.runTask("t5", pods); got != idle {
		t.Fatalf("t5 went to %s, want the idle pod %s", got.id, idle.id)
	}
	if n := len(h2.kubePods()); n > 4 {
		t.Fatalf("pods after the restart = %d, want at most the budget of 4", n)
	}
}

// A task placed on a pod, or handed to it, that had not started when the
// manager stopped is handed to that pod again by the restarted manager, runs
// and is not failed.
func TestARestartedManagerHandsATaskNotStartedAgain(t *testing.T) {
	for _, handed := range []bool{false, true} {
		name := "placed"
		if handed {
			name = "handed"
		}
		t.Run(name, func(t *testing.T) {
			w := newKubeWorld(t)
			h1 := w.manager(1, 2)
			pods := h1.startedPods(1)
			w.upstream.enqueueIssue("t1", "issue-1")
			h1.dispatch()
			want := core.TaskPlaced
			if handed {
				want = core.TaskHanded
				if got := pods[0].pollBatch(); !slices.Equal(got, []string{"t1"}) {
					t.Fatalf("the pod got %v, want t1", got)
				}
			}
			if st := w.store.task("t1").State; st != want {
				t.Fatalf("t1 is %q in the record, want %q", st, want)
			}
			h1.stop()

			h2 := w.manager(1, 2)
			pods[0].h = h2
			if got := pods[0].pollBatch(); !slices.Equal(got, []string{"t1"}) {
				t.Fatalf("after the restart the pod got %v, want t1", got)
			}
			if code := pods[0].report("t1", "start"); code != http.StatusOK {
				t.Fatalf("t1's start after the restart: %d", code)
			}
			if got := pods[0].pollBatch(); len(got) != 0 {
				t.Fatalf("the pod got %v once t1 started, want nothing", got)
			}
			if code := pods[0].report("t1", "complete"); code != http.StatusOK {
				t.Fatalf("t1's completion after the restart: %d", code)
			}
			h2.wantEnded("t1", "pod reported complete")
			if why, failed := w.upstream.failureOf("t1"); failed {
				t.Fatalf("the restart failed t1: %s", why)
			}
		})
	}
}
