package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"sandbox-manager/internal/sandbox"
)

// newRestoreHarness is newKubeHarness without Run: the caller restores
// first, as the manager does on start.
func newRestoreHarness(t *testing.T, cfg sandbox.KubeConfig, objects ...runtime.Object) *kubeHarness {
	t.Helper()
	h := &kubeHarness{t: t, clock: &kubeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}, notes: &kubeNotes{}}
	h.client = newFakeClient(objects...)
	cfg.Client, cfg.Namespace, cfg.Now, cfg.Notify = h.client, kubeNS, h.clock.Now, h.notes
	cfg.TickInterval = 5 * time.Millisecond
	if cfg.Template == nil {
		cfg.Template = kubeTemplate()
	}
	svc, err := sandbox.NewKube(cfg)
	if err != nil {
		t.Fatalf("NewKube: %v", err)
	}
	h.svc = svc
	return h
}

func (h *kubeHarness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.t.Cleanup(cancel)
	go h.svc.Run(ctx)
}

func (h *kubeHarness) pod(name string) (corev1.Pod, bool) {
	for _, pd := range h.pods() {
		if pd.Name == name {
			return pd, true
		}
	}
	return corev1.Pod{}, false
}

// leftPod is a pod a previous run created, with its pod token and what that
// run recorded on it.
func leftPod(name, ticket string, labels, annotations map[string]string) *corev1.Pod {
	l := map[string]string{sandbox.ManagedByLabel: sandbox.ManagedByValue}
	for k, v := range labels {
		l[k] = v
	}
	pd := kubeTemplate()
	pd.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: kubeNS, Labels: l, Annotations: annotations,
		CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC))}
	if ticket != "" {
		pd.Spec.Containers[1].Env = []corev1.EnvVar{{Name: sandbox.PodTicketEnv, Value: ticket}}
	} else {
		pd.Spec.Containers[1].Env = nil
	}
	pd.Status.Phase = corev1.PodRunning
	return pd
}

func TestKubeRecordsWhatAPodServesOnThePod(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 2})
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	h.ready()
	id, err := h.svc.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	started := time.Date(2026, 11, 16, 9, 1, 0, 0, time.UTC)
	h.svc.Record(id, sandbox.PodRecord{Phase: "running", Tenant: "ws-1", Kind: "pool", Key: "issue-1", Task: "task-1", TaskStarted: started})
	h.eventually("the running task on the pod", func() bool {
		pd, _ := h.pod(string(id))
		return pd.Annotations[sandbox.TaskAnnotation] == "task-1"
	})
	pd, _ := h.pod(string(id))
	if pd.Labels[sandbox.WorkspaceLabel] != "ws-1" || pd.Labels[sandbox.KindLabel] != "pool" ||
		pd.Annotations[sandbox.KeyAnnotation] != "issue-1" || pd.Annotations[sandbox.PhaseAnnotation] != "running" ||
		pd.Annotations[sandbox.TaskStartedAnnotation] != started.Format(time.RFC3339Nano) {
		t.Fatalf("labels %v, annotations %v: want the workspace, kind, key, phase and task start", pd.Labels, pd.Annotations)
	}
	if pd.Labels[sandbox.ManagedByLabel] != sandbox.ManagedByValue || pd.Labels["team"] != "ai" {
		t.Fatalf("labels %v lost the manager's or the template's", pd.Labels)
	}

	until := started.Add(10 * time.Minute)
	h.svc.Record(id, sandbox.PodRecord{Phase: "held", Tenant: "ws-1", Kind: "pool", Key: "issue-1", Since: started, Until: until})
	h.eventually("the hold on the pod", func() bool {
		pd, _ := h.pod(string(id))
		return pd.Annotations[sandbox.PhaseAnnotation] == "held"
	})
	pd, _ = h.pod(string(id))
	if _, ok := pd.Annotations[sandbox.TaskAnnotation]; ok {
		t.Fatalf("annotations %v still name the ended task", pd.Annotations)
	}
	if pd.Annotations[sandbox.UntilAnnotation] != until.Format(time.RFC3339Nano) {
		t.Fatalf("annotations %v: want the hold's end", pd.Annotations)
	}
}

// Without records a restart fails every running task, so a refused record
// halts the service, as refused credentials do: the refusal shows, no pod is
// given out and Halted says why, until a record is written again.
func TestKubeHaltsWhileRecordsAreRefused(t *testing.T) {
	h := newRestoreHarness(t, sandbox.KubeConfig{WarmPods: 2, MaxPods: 2})
	var mu sync.Mutex
	refuse := false
	h.client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if !refuse {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no patch"))
	})
	h.run()
	h.eventually("two warm pods", func() bool { return len(h.pods()) == 2 })
	h.ready()
	id, err := h.svc.Create(context.Background())
	if err != nil {
		t.Fatalf("Create before any refusal: %v", err)
	}
	mu.Lock()
	refuse = true
	mu.Unlock()
	h.svc.Record(id, sandbox.PodRecord{Phase: "running", Tenant: "ws-1", Kind: "pool", Task: "task-1", TaskStarted: h.clock.Now()})
	h.eventually("the refusal shown", func() bool {
		p := h.svc.Inventory().Problem
		return contains(p, "refused") && contains(p, "record")
	})
	if why := h.svc.Halted(); !contains(why, "refused") {
		t.Fatalf("Halted = %q, want the refusal", why)
	}
	if _, err := h.svc.Create(context.Background()); !errors.Is(err, sandbox.ErrNoCapacity) || !contains(err.Error(), "refused") {
		t.Fatalf("Create = %v, want ErrNoCapacity naming the refusal", err)
	}

	mu.Lock()
	refuse = false
	mu.Unlock()
	h.eventually("the record written once allowed", func() bool {
		pd, _ := h.pod(string(id))
		return pd.Annotations[sandbox.TaskAnnotation] == "task-1"
	})
	if why := h.svc.Halted(); why != "" {
		t.Fatalf("Halted = %q after the record was written", why)
	}
	if _, err := h.svc.Create(context.Background()); err != nil {
		t.Fatalf("Create after the record was written: %v", err)
	}
}

// A refusal found on start halts the service even with no pod to record on.
func TestKubeRestoreWithoutTheRightToRecordHalts(t *testing.T) {
	h := newRestoreHarness(t, sandbox.KubeConfig{MaxPods: 2})
	h.client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no patch"))
	})
	if _, err := h.svc.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if why := h.svc.Halted(); !contains(why, "patch") {
		t.Fatalf("Halted = %q, want the missing right named", why)
	}
}

// A record that could not be written is written again until it is: a
// restart must find what a pod serves.
func TestKubeWritesARecordAgainUntilItIsWritten(t *testing.T) {
	h := newRestoreHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 1})
	var mu sync.Mutex
	failures := 3
	h.client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if failures == 0 {
			return false, nil, nil
		}
		failures--
		return true, nil, apierrors.NewServiceUnavailable("try later")
	})
	h.run()
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	h.ready()
	id, _ := h.svc.Create(context.Background())
	h.svc.Record(id, sandbox.PodRecord{Phase: "running", Tenant: "ws-1", Kind: "pool", Task: "task-1", TaskStarted: h.clock.Now()})
	h.eventually("the record written after the failures", func() bool {
		pd, _ := h.pod(string(id))
		return pd.Annotations[sandbox.TaskAnnotation] == "task-1"
	})
}

func TestKubeRebuildsFromThePodsItFinds(t *testing.T) {
	at := func(m int) string { return time.Date(2026, 11, 16, 8, m, 0, 0, time.UTC).Format(time.RFC3339Nano) }
	running := leftPod("sandbox-run", "mpt_run", map[string]string{sandbox.WorkspaceLabel: "ws-1", sandbox.KindLabel: "pool"},
		map[string]string{sandbox.PhaseAnnotation: "running", sandbox.KeyAnnotation: "issue-1", sandbox.TaskAnnotation: "task-1", sandbox.TaskStartedAnnotation: at(30)})
	held := leftPod("sandbox-held", "mpt_held", map[string]string{sandbox.WorkspaceLabel: "ws-1", sandbox.KindLabel: "clean"},
		map[string]string{sandbox.PhaseAnnotation: "held", sandbox.KeyAnnotation: "issue-2", sandbox.SinceAnnotation: at(55), sandbox.UntilAnnotation: "2026-11-16T09:05:00Z"})
	idle := leftPod("sandbox-idle", "mpt_idle", map[string]string{sandbox.WorkspaceLabel: "ws-2", sandbox.KindLabel: "pool"},
		map[string]string{sandbox.PhaseAnnotation: "idle", sandbox.SinceAnnotation: at(40)})
	warm := leftPod("sandbox-warm", "mpt_warm", nil, nil)
	warm.Status.Phase = corev1.PodPending
	noToken := leftPod("sandbox-notoken", "", nil, nil)
	garbled := leftPod("sandbox-garbled", "mpt_garbled", map[string]string{sandbox.WorkspaceLabel: "ws-1"},
		map[string]string{sandbox.PhaseAnnotation: "dancing"})
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "multica-backend", Namespace: kubeNS}}
	h := newRestoreHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 5}, running, held, idle, warm, noToken, garbled, other)

	got, err := h.svc.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	byID := map[sandbox.PodID]sandbox.PodRecord{}
	for _, r := range got {
		byID[r.ID] = r.Record
	}
	if len(byID) != 3 {
		t.Fatalf("restored %v, want the running, held and idle pods", got)
	}
	if r := byID["sandbox-run"]; r.Phase != "running" || r.Tenant != "ws-1" || r.Kind != "pool" || r.Key != "issue-1" || r.Task != "task-1" ||
		!r.TaskStarted.Equal(time.Date(2026, 11, 16, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("running pod = %+v", r)
	}
	if r := byID["sandbox-held"]; r.Phase != "held" || r.Kind != "clean" || r.Key != "issue-2" || !r.Until.Equal(time.Date(2026, 11, 16, 9, 5, 0, 0, time.UTC)) {
		t.Errorf("held pod = %+v", r)
	}
	if r := byID["sandbox-idle"]; r.Phase != "idle" || r.Tenant != "ws-2" || !r.Since.Equal(time.Date(2026, 11, 16, 8, 40, 0, 0, time.UTC)) {
		t.Errorf("idle pod = %+v", r)
	}
	for _, name := range []sandbox.PodID{"sandbox-run", "sandbox-held", "sandbox-idle"} {
		if info, ok := h.svc.Pod(name); !ok || info.State != sandbox.PodHeld {
			t.Errorf("pod %s = %+v, want held by the caller", name, info)
		}
	}
	if id, ok := h.svc.Authenticate("mpt_run"); !ok || id != "sandbox-run" {
		t.Errorf("the running pod's token = %q, %v", id, ok)
	}

	h.run()
	h.eventually("the pods it cannot interpret deleted", func() bool {
		_, a := h.pod("sandbox-notoken")
		_, b := h.pod("sandbox-garbled")
		return !a && !b
	})
	time.Sleep(30 * time.Millisecond)
	for _, name := range []string{"sandbox-run", "sandbox-held", "sandbox-idle", "sandbox-warm", "multica-backend"} {
		if _, ok := h.pod(name); !ok {
			t.Errorf("pod %s was deleted", name)
		}
	}
	if n := len(h.pods()); n != 5 {
		t.Errorf("pods = %d, want no new one while the kept warm pod counts", n)
	}
	if id, ok := h.svc.Authenticate("mpt_warm"); !ok || id != "sandbox-warm" {
		t.Fatalf("the warm pod's token = %q, %v", id, ok)
	}
	if id, err := h.svc.Create(context.Background()); err != nil || id != "sandbox-warm" {
		t.Fatalf("Create = %q, %v; want the kept warm pod", id, err)
	}
}

// A pod with no record may have been given a task whose record was never
// written: only a pod whose containers never started is surely warm. Every
// other unrecorded pod is deleted, so it never serves another key while it
// still runs the old task.
func TestKubeRestoreDeletesUnrecordedPodsThatMayRunATask(t *testing.T) {
	started := leftPod("sandbox-started", "mpt_started", nil, nil)
	started.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	pendingButStarted := leftPod("sandbox-sidecar-wait", "mpt_sidecar", nil, nil)
	pendingButStarted.Status.Phase = corev1.PodPending
	pendingButStarted.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	fresh := leftPod("sandbox-fresh", "mpt_fresh", nil, nil)
	fresh.Status.Phase = corev1.PodPending
	fresh.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}}
	h := newRestoreHarness(t, sandbox.KubeConfig{MaxPods: 3}, started, pendingButStarted, fresh)
	got, err := h.svc.Restore(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("Restore = %v, %v; want nothing serving anyone", got, err)
	}
	for _, name := range []string{"sandbox-started", "sandbox-sidecar-wait"} {
		if _, ok := h.pod(name); ok {
			t.Errorf("unrecorded pod %s that may run a task was kept", name)
		}
	}
	if _, ok := h.pod("sandbox-fresh"); !ok {
		t.Fatal("the pod that never started was deleted")
	}
	if _, ok := h.svc.Authenticate("mpt_started"); ok {
		t.Error("the deleted pod's token is still accepted")
	}
	if id, ok := h.svc.Authenticate("mpt_fresh"); !ok || id != "sandbox-fresh" {
		t.Fatalf("the fresh pod's token = %q, %v", id, ok)
	}
}

func TestKubeRestoredPodThatEndedMeanwhileIsLost(t *testing.T) {
	gone := leftPod("sandbox-oom", "mpt_oom", map[string]string{sandbox.WorkspaceLabel: "ws-1", sandbox.KindLabel: "pool"},
		map[string]string{sandbox.PhaseAnnotation: "running", sandbox.TaskAnnotation: "task-1", sandbox.TaskStartedAnnotation: "2026-11-16T08:30:00Z"})
	gone.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}}
	h := newRestoreHarness(t, sandbox.KubeConfig{MaxPods: 2}, gone)
	got, err := h.svc.Restore(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Restore = %v, %v; want the pod and its task", got, err)
	}
	h.run()
	h.eventually("the loss noted", func() bool { return len(h.notes.all()) == 1 })
	if n := h.notes.all()[0]; n.id != "sandbox-oom" || !strings.HasPrefix(n.why, "the pod ran out of memory (limit 4Gi). ") {
		t.Fatalf("lost = %+v", n)
	}
}

func TestKubeRestoreNamesRefusedCredentials(t *testing.T) {
	h := newRestoreHarness(t, sandbox.KubeConfig{})
	h.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no list"))
	})
	if _, err := h.svc.Restore(context.Background()); err == nil || !contains(err.Error(), "credentials") {
		t.Fatalf("Restore = %v, want an error naming the credentials", err)
	}
}

func newFakeClient(objects ...runtime.Object) *fake.Clientset {
	return fake.NewSimpleClientset(objects...)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Without the right to patch pods no record was ever written, so a pod with
// none may have served anyone: every pod a previous run left is deleted.
func TestKubeRestoreWithoutTheRightToRecordDeletesEveryPod(t *testing.T) {
	served := leftPod("sandbox-run", "mpt_run", map[string]string{sandbox.WorkspaceLabel: "ws-1", sandbox.KindLabel: "pool"},
		map[string]string{sandbox.PhaseAnnotation: "running", sandbox.TaskAnnotation: "task-1", sandbox.TaskStartedAnnotation: "2026-11-16T08:30:00Z"})
	unrecorded := leftPod("sandbox-warm", "mpt_warm", nil, nil)
	h := newRestoreHarness(t, sandbox.KubeConfig{MaxPods: 2}, served, unrecorded)
	h.client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no patch"))
	})
	got, err := h.svc.Restore(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("Restore = %v, %v; want nothing restored", got, err)
	}
	if n := len(h.pods()); n != 0 {
		t.Fatalf("pods left = %d, want every one deleted", n)
	}
	if p := h.svc.Inventory().Problem; !contains(p, "patch") {
		t.Fatalf("problem = %q, want the missing right named", p)
	}
}
