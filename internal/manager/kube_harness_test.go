package manager_test

// The harness on the Kubernetes sandbox service: the manager creates its pods
// in a fake Kubernetes API, and each simulated pod presents the pod token the
// service put in its environment.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/sandbox"
)

const kubeNS = "sandbox-test"

// kubeWorld is what outlives one manager: the Kubernetes namespace, the
// Multica server and the store of the dispatch record.
type kubeWorld struct {
	t        *testing.T
	client   *fake.Clientset
	upstream *fakeMultica
	store    *memStore
}

func newKubeWorld(t *testing.T) *kubeWorld {
	t.Helper()
	return &kubeWorld{t: t, client: fake.NewSimpleClientset(), upstream: newFakeMultica(t), store: newMemStore()}
}

func podTemplate() *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "daemon", Image: "pod:test",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
	}}}}
}

// manager starts a manager on the world, as the command line does: it takes
// over the pods a previous one left, finds its workspaces, then runs its pod
// side. Its stop ends it.
func (w *kubeWorld) manager(warm, maxPods int, opts ...func(*manager.Config)) *harness {
	w.t.Helper()
	cfg := manager.Config{MulticaURL: w.upstream.URL, PAT: harnessPAT, Pool: multica.Machine{Name: "Test Pool"}, Store: w.store}
	for _, opt := range opts {
		opt(&cfg)
	}
	h := startManager(w.t, w.client, cfg, warm, maxPods, func(ctx context.Context, m *manager.Manager) error {
		_, err := m.Sync(ctx)
		return err
	})
	h.upstream = w.upstream
	return h
}

// startManager starts a manager with cfg on the Kubernetes API client, its
// sandbox service keeping warm pods within maxPods: it takes over the pods a
// previous one left, finds its workspaces with serve, then runs its pod
// side. Its stop ends it.
func startManager(t *testing.T, client *fake.Clientset, cfg manager.Config, warm, maxPods int, serve func(context.Context, *manager.Manager) error) *harness {
	t.Helper()
	cfg.Kube = &sandbox.KubeConfig{
		Client: client, Namespace: kubeNS, Template: podTemplate(),
		WarmPods: warm, MaxPods: maxPods, TickInterval: 5 * time.Millisecond,
		// Tests move the clock by hours; a pod that has not called in yet
		// must not time out meanwhile.
		StartTimeout: 1000 * time.Hour,
	}
	logs, audit := &syncBuffer{}, &syncBuffer{}
	cfg.AuditLog = audit
	if cfg.Store == nil {
		cfg.Store = newMemStore()
	}
	m, err := manager.New(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := serve(ctx, m); err != nil {
		t.Fatal(err)
	}
	go m.RunPods(ctx)
	stop := func() {
		cancel()
		if err := m.Flush(context.Background()); err != nil {
			t.Errorf("write the dispatch record as the manager stops: %v", err)
		}
	}
	h := &harness{t: t, pool: m, logs: logs, audit: audit, kube: client, stop: stop, adopted: map[string]bool{}}
	h.podAPI = httptest.NewServer(m.PodHandler())
	h.admin = httptest.NewServer(m.AdminHandler())
	t.Cleanup(h.podAPI.Close)
	t.Cleanup(h.admin.Close)
	return h
}

// testKube is a sandbox service on a fake Kubernetes API of its own, for a
// manager that is only built.
func testKube() *sandbox.KubeConfig {
	return &sandbox.KubeConfig{Client: fake.NewSimpleClientset(), Namespace: kubeNS, Template: podTemplate()}
}

// kubePods lists the pods in the namespace.
func (h *harness) kubePods() []corev1.Pod {
	h.t.Helper()
	list, err := h.kube.CoreV1().Pods(kubeNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return list.Items
}

func (h *harness) kubePod(name string) corev1.Pod {
	h.t.Helper()
	pd, err := h.kube.CoreV1().Pods(kubeNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return *pd
}

// kubeHas reports whether the pod exists in Kubernetes.
func (h *harness) kubeHas(name string) bool {
	_, err := h.kube.CoreV1().Pods(kubeNS).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

// adopt plays the daemon of a pod the service created, with its pod token.
func (h *harness) adopt(pd corev1.Pod) *simPod {
	h.t.Helper()
	transport := &http.Transport{}
	h.t.Cleanup(transport.CloseIdleConnections)
	p := &simPod{h: h, name: pd.Name, id: pd.Name, client: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
	for _, e := range pd.Spec.Containers[0].Env {
		if e.Name == sandbox.PodTicketEnv {
			p.ticket = e.Value
		}
	}
	if p.ticket == "" {
		h.t.Fatalf("pod %s has no pod token", pd.Name)
	}
	return p
}

// startedPods waits for n pods and starts their daemons.
func (h *harness) startedPods(n int) []*simPod {
	h.t.Helper()
	eventually(h.t, "the warm pods", func() bool { return len(h.kubePods()) >= n })
	var out []*simPod
	for range n {
		p := h.newPod("")
		p.start()
		out = append(out, p)
	}
	return out
}

// runTask claims a queued task and returns the pod that picked it up.
func (h *harness) runTask(id string, pods []*simPod) *simPod {
	h.t.Helper()
	h.dispatch()
	for _, p := range pods {
		if slices.Contains(p.pollBatch(), id) {
			return p
		}
	}
	h.t.Fatalf("no pod picked up %s", id)
	return nil
}

// enqueueIssue adds a task of the given issue.
func (f *fakeMultica) enqueueIssue(id, issue string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": issue,
	})
}
