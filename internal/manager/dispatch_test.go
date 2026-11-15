package manager_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"sandbox-manager/internal/manager"
)

func (h *harness) dispatchView() manager.DispatchView {
	h.t.Helper()
	resp, err := http.Get(h.admin.URL + "/v1/dispatch")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("dispatch status %d", resp.StatusCode)
	}
	var v manager.DispatchView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

func TestDispatchViewShowsTargetAndActualPods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	h.newPod("pool-1").start()

	v := h.dispatchView()
	if v.Pool.Live != 2 || v.Pool.Max != 2 || v.Pool.Running != 1 || v.Pool.Warm != 1 {
		t.Fatalf("pool %+v, want 2 live of 2, one running and one warm", v.Pool)
	}
	if len(v.Workspaces) != 1 {
		t.Fatalf("workspaces %+v", v.Workspaces)
	}
	w := v.Workspaces[0]
	if w.OccupiedPods != 1 || w.RunningPods != 1 || w.HoldingPods != 0 || w.TargetPods != 1 || w.Leased != 1 || w.Weight != 1 {
		t.Fatalf("workspace share %+v", w)
	}
	if v.Totals.OccupiedPods != 1 || v.Totals.UnoccupiedPods != 1 {
		t.Fatalf("totals %+v", v.Totals)
	}
	if len(v.Pods) != 2 {
		t.Fatalf("pods %+v", v.Pods)
	}
	byPhase := map[string]manager.DispatchPod{}
	for _, p := range v.Pods {
		byPhase[p.Phase] = p
	}
	if p := byPhase[manager.PodRunning]; p.ID != pod.id || p.WorkspaceID != w.WorkspaceID || p.Kind != "pool" || p.Key != "issue:issue-t1" || p.TaskID != "t1" {
		t.Fatalf("the running pod %+v", p)
	}
	if p, ok := byPhase[manager.PodWarm]; !ok || p.WorkspaceID != "" || p.Kind != "" {
		t.Fatalf("the warm pod %+v", p)
	}
}

// The dispatch view shows each pod in its phase, a failed pod with its
// reason, and why pods cannot be created.
func TestDispatchViewShowsEveryPodPhaseAndCreationFailures(t *testing.T) {
	t.Parallel()
	w := newKubeWorld(t)
	var full atomic.Bool
	w.client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if full.Load() {
			return true, nil, errors.New("0/1 nodes are available: 1 Insufficient memory")
		}
		return false, nil, nil
	})
	h := w.manager(6, 6, holding(time.Hour))
	pods := h.startedPods(5) // the sixth never calls in: starting

	h.upstream.enqueueIssue("t1", "issue-1")
	running := h.runTask("t1", pods)
	h.upstream.enqueueIssue("t4", "issue-4")
	doomed := h.runTask("t4", pods)
	h.upstream.enqueueIssue("t2", "issue-2")
	held := h.runTask("t2", pods)
	held.report("t2", "complete")
	h.upstream.enqueueNoInitiator("t3")
	idle := h.runTask("t3", pods)
	idle.report("t3", "complete")

	// The node has no room for the pod that would replace the failed one.
	full.Store(true)
	h.setPodStatus(doomed.id, corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}})
	eventually(t, "t4 failed", func() bool { _, ok := h.upstream.failureOf("t4"); return ok })
	if why, _ := h.upstream.failureOf("t4"); why != "the pod running this task was lost: the pod ran out of memory (limit 1Gi)" {
		t.Fatalf("t4 failed with %q", why)
	}
	eventually(t, "the creation failure shown", func() bool { return h.dispatchView().Pool.Problem != "" })

	v := h.dispatchView()
	phase := map[string]manager.DispatchPod{}
	for _, p := range v.Pods {
		phase[p.ID] = p
	}
	for p, want := range map[*simPod]string{running: manager.PodRunning, held: manager.PodHeld, idle: manager.PodIdle, doomed: manager.PodFailed} {
		if got := phase[p.id].Phase; got != want {
			t.Errorf("pod %s: %s, want %s", p.id, got, want)
		}
	}
	if p := phase[held.id]; p.Key != "issue:issue-2" || p.HoldRemainingSeconds <= 0 || p.TaskID != "" {
		t.Errorf("the held pod %+v, want its key and its hold's countdown", p)
	}
	if p := phase[doomed.id]; !strings.Contains(p.Reason, "out of memory") {
		t.Errorf("the failed pod %+v, want its reason", p)
	}
	if v.Pool.Running != 1 || v.Pool.Held != 1 || v.Pool.Idle != 1 || v.Pool.Failed != 1 || v.Pool.Warm != 1 || v.Pool.Starting != 1 {
		t.Errorf("pool %+v, want one pod in each phase", v.Pool)
	}
	if !strings.Contains(v.Pool.Problem, "Insufficient memory") {
		t.Errorf("problem %q, want the creation failure's reason", v.Pool.Problem)
	}
}

func TestDispatchPageReadsTheViewItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	resp, err := http.Get(h.admin.URL + "/dispatch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	page := string(body)
	if !strings.Contains(page, `fetch("v1/dispatch"`) || !strings.Contains(page, "10000") {
		t.Fatal("the page must read /v1/dispatch every 10 seconds")
	}
	for _, shown := range []string{"warm", "starting", "running", "held", "idle", "failed", "Pods cannot be created or started", "p.reason", "p.kind"} {
		if !strings.Contains(page, shown) {
			t.Errorf("the page does not show %q", shown)
		}
	}
	for _, gone := range []string{"resetting", "slots", "binding"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still shows %q", gone)
		}
	}
}

func TestDispatchViewListsEachWorkspacesTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	first := h.newPod("pool-0")
	leaseOne(t, h, first, "t1")
	if code := first.report("t1", "complete"); code != http.StatusOK {
		t.Fatalf("complete: %d", code)
	}
	second := h.newPod("pool-1")
	h.upstream.enqueue("t2")
	second.start()
	h.runTask("t2", []*simPod{first, second})
	h.upstream.enqueue("t3")
	h.dispatch()

	v := h.dispatchView()
	states := map[string]manager.DispatchTask{}
	for _, task := range v.Workspaces[0].Tasks {
		states[task.ID] = task
	}
	if s := states["t1"]; s.State != manager.TaskEnded || s.EndReason == "" || s.StartedAt == nil || s.PodName != first.id {
		t.Fatalf("t1 %+v, want ended on %s with its reason", s, first.id)
	}
	if s := states["t2"]; s.State != manager.TaskRunning || s.PodName == "" {
		t.Fatalf("t2 %+v, want running on a pod", s)
	}
	if s := states["t3"]; s.State == "" || s.State == manager.TaskEnded {
		t.Fatalf("t3 %+v, want it waiting or under way", s)
	}
}

// setPodStatus sets a pod's status in Kubernetes, as the kubelet would.
func (h *harness) setPodStatus(name string, st corev1.PodStatus) {
	h.t.Helper()
	pd, err := h.kube.CoreV1().Pods(kubeNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	pd.Status = st
	if _, err := h.kube.CoreV1().Pods(kubeNS).UpdateStatus(context.Background(), pd, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}
