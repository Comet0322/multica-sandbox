package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"sandbox-manager/internal/sandbox"
)

const kubeNS = "multica-sandbox"

func kubeTemplate() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ignored", Labels: map[string]string{"team": "ai"}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{Name: "sidecar", Image: "sidecar:1"},
				{
					Name: "daemon", Image: "pod:1",
					Env: []corev1.EnvVar{{Name: "KEEP", Value: "1"}, {Name: sandbox.PodTokenEnv, Value: "from-template"}},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
					},
				},
			},
		},
	}
}

type lostNote struct {
	id  sandbox.PodID
	why string
}

type kubeNotes struct {
	mu   sync.Mutex
	lost []lostNote
}

func (n *kubeNotes) Interrupted(sandbox.PodID, string) {}
func (n *kubeNotes) Lost(id sandbox.PodID, why string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lost = append(n.lost, lostNote{id, why})
}
func (n *kubeNotes) all() []lostNote {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]lostNote(nil), n.lost...)
}

type kubeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *kubeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *kubeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type kubeHarness struct {
	t      *testing.T
	client *fake.Clientset
	svc    *sandbox.KubeService
	clock  *kubeClock
	notes  *kubeNotes
	cancel context.CancelFunc
}

func newKubeHarness(t *testing.T, cfg sandbox.KubeConfig, objects ...runtime.Object) *kubeHarness {
	t.Helper()
	h := &kubeHarness{t: t, client: fake.NewSimpleClientset(objects...), clock: &kubeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}, notes: &kubeNotes{}}
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
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(cancel)
	go svc.Run(ctx)
	return h
}

func (h *kubeHarness) eventually(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *kubeHarness) pods() []corev1.Pod {
	list, err := h.client.CoreV1().Pods(kubeNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		h.t.Fatalf("list pods: %v", err)
	}
	return list.Items
}

func tokenOf(pd corev1.Pod) string {
	for _, c := range pd.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == sandbox.PodTokenEnv {
				return e.Value
			}
		}
	}
	return ""
}

// ready makes every created pod's daemon call in, and returns their names.
func (h *kubeHarness) ready() []string {
	var names []string
	for _, pd := range h.pods() {
		if _, ok := h.svc.Authenticate(tokenOf(pd)); ok {
			names = append(names, pd.Name)
		}
	}
	return names
}

func (h *kubeHarness) setStatus(name string, st corev1.PodStatus) {
	h.t.Helper()
	pd, err := h.client.CoreV1().Pods(kubeNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("get pod %s: %v", name, err)
	}
	pd.Status = st
	if _, err := h.client.CoreV1().Pods(kubeNS).UpdateStatus(context.Background(), pd, metav1.UpdateOptions{}); err != nil {
		h.t.Fatalf("update status of %s: %v", name, err)
	}
}

func TestKubeCreatesWarmPodsFromTheTemplate(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 2, MaxPods: 3})
	h.eventually("two warm pods", func() bool { return len(h.pods()) == 2 })
	for _, pd := range h.pods() {
		if pd.Labels[sandbox.ManagedByLabel] != sandbox.ManagedByValue || pd.Labels["team"] != "ai" {
			t.Errorf("pod %s labels = %v, want the manager's and the template's", pd.Name, pd.Labels)
		}
		if pd.Name == "ignored" || !strings.HasPrefix(pd.Name, "sandbox-") {
			t.Errorf("pod name = %q, want one the service chose", pd.Name)
		}
		if pd.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("restart policy = %s, want Never", pd.Spec.RestartPolicy)
		}
		daemon := pd.Spec.Containers[1]
		var tokens []string
		keep := false
		for _, e := range daemon.Env {
			if e.Name == sandbox.PodTokenEnv {
				tokens = append(tokens, e.Value)
			}
			keep = keep || e.Name == "KEEP"
		}
		if len(tokens) != 1 || !strings.HasPrefix(tokens[0], "mpt_") || !keep {
			t.Errorf("daemon env = %v, want KEEP and one issued pod token", daemon.Env)
		}
		if len(pd.Spec.Containers[0].Env) != 0 {
			t.Errorf("the sidecar got env %v, want none", pd.Spec.Containers[0].Env)
		}
	}
	if _, err := h.svc.Create(context.Background()); !errors.Is(err, sandbox.ErrNoCapacity) {
		t.Fatalf("Create before any pod is ready = %v, want ErrNoCapacity", err)
	}
}

func TestKubeLendsAPodOnceItsDaemonCallsIn(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 2})
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	names := h.ready()
	if len(names) != 1 {
		t.Fatalf("ready pods = %v, want one", names)
	}
	id, err := h.svc.Create(context.Background())
	if err != nil || string(id) != names[0] {
		t.Fatalf("Create = %q, %v; want %s", id, err, names[0])
	}
	if info, _ := h.svc.Pod(id); info.State != sandbox.PodHeld {
		t.Fatalf("state = %s, want held", info.State)
	}
	h.eventually("a new warm pod in its place", func() bool { return len(h.pods()) == 2 })
	if err := h.svc.Delete(context.Background(), id, sandbox.AuditLabel{Reason: "task done"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	h.eventually("the pod deleted in Kubernetes", func() bool {
		for _, pd := range h.pods() {
			if pd.Name == string(id) {
				return false
			}
		}
		return true
	})
	if _, ok := h.svc.Authenticate(tokenOf(corev1.Pod{})); ok {
		t.Fatal("an empty token authenticated")
	}
}

func TestKubeCreatesAPodOnDemandWithNoWarmPods(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{MaxPods: 1})
	time.Sleep(30 * time.Millisecond)
	if n := len(h.pods()); n != 0 {
		t.Fatalf("pods with no warm count = %d, want 0", n)
	}
	if _, err := h.svc.Create(context.Background()); !errors.Is(err, sandbox.ErrNoCapacity) {
		t.Fatalf("Create = %v, want ErrNoCapacity", err)
	}
	h.eventually("a pod created for the demand", func() bool { return len(h.pods()) == 1 })
	h.ready()
	if _, err := h.svc.Create(context.Background()); err != nil {
		t.Fatalf("Create after the pod got ready: %v", err)
	}
	if _, err := h.svc.Create(context.Background()); !errors.Is(err, sandbox.ErrNoCapacity) {
		t.Fatalf("Create past the most pods = %v, want ErrNoCapacity", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(h.pods()); n != 1 {
		t.Fatalf("pods = %d, want no more than the most (1)", n)
	}
}

func TestKubeDeletesAPodThatDoesNotStartAndBacksOff(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1, StartTimeout: time.Minute})
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	name := h.pods()[0].Name
	h.setStatus(name, corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pod:1 not found"}},
	}}})
	h.eventually("the waiting reason observed", func() bool { return true })
	time.Sleep(30 * time.Millisecond)
	h.clock.Add(2 * time.Minute)
	h.eventually("the pod deleted", func() bool { return len(h.pods()) == 0 })
	inv := h.svc.Inventory()
	if !strings.Contains(inv.Problem, "did not start within 1m0s") || !strings.Contains(inv.Problem, "ImagePullBackOff") {
		t.Fatalf("problem = %q, want the timeout and the waiting reason", inv.Problem)
	}
	_, err := h.svc.Create(context.Background())
	if !errors.Is(err, sandbox.ErrNoCapacity) || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("Create = %v, want ErrNoCapacity with the reason", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(h.pods()); n != 0 {
		t.Fatalf("pods during the backoff = %d, want 0", n)
	}
	h.clock.Add(15 * time.Second)
	h.eventually("pods created after the backoff", func() bool { return len(h.pods()) >= 1 })
}

func TestKubeReportsHowAHeldPodWasLost(t *testing.T) {
	cases := []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{"out of memory", corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "daemon", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}}}, "the pod ran out of memory (limit 4Gi)"},
		{"evicted", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "low on memory"}, "the node evicted the pod: low on memory"},
		{"daemon exited", corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "daemon", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
		}}}, "the pod's daemon container exited with code 1 (Error)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 1})
			h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
			h.ready()
			id, err := h.svc.Create(context.Background())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			h.setStatus(string(id), tc.status)
			h.eventually("the loss noted", func() bool { return len(h.notes.all()) == 1 })
			if got := h.notes.all()[0]; got.id != id || got.why != tc.want {
				t.Fatalf("lost = %+v, want %s: %q", got, id, tc.want)
			}
			h.eventually("the pod deleted", func() bool { return len(h.pods()) == 0 || h.pods()[0].Name != string(id) })
		})
	}
}

func TestKubeNotesAHeldPodDeletedOutsideTheManager(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 1})
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	h.ready()
	id, _ := h.svc.Create(context.Background())
	if err := h.client.CoreV1().Pods(kubeNS).Delete(context.Background(), string(id), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.eventually("the loss noted", func() bool { return len(h.notes.all()) == 1 })
	if got := h.notes.all()[0].why; got != "the pod was deleted" {
		t.Fatalf("why = %q", got)
	}
}

func TestKubeShowsRefusedCredentials(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no rights"))
	})
	clock := &kubeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	svc, err := sandbox.NewKube(sandbox.KubeConfig{Client: client, Namespace: kubeNS, Template: kubeTemplate(), WarmPods: 1, Now: clock.Now, TickInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(svc.Inventory().Problem, "refused the manager's credentials") {
		if time.Now().After(deadline) {
			t.Fatalf("problem = %q, want refused credentials", svc.Inventory().Problem)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := svc.Create(ctx); !errors.Is(err, sandbox.ErrNoCapacity) || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("Create = %v, want ErrNoCapacity naming the credentials", err)
	}
}

func TestKubeDeletesPodsAPreviousRunLeft(t *testing.T) {
	left := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-old", Namespace: kubeNS, Labels: map[string]string{sandbox.ManagedByLabel: sandbox.ManagedByValue}}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "multica-backend", Namespace: kubeNS}}
	h := newKubeHarness(t, sandbox.KubeConfig{}, left, other)
	h.eventually("the leftover deleted", func() bool {
		for _, pd := range h.pods() {
			if pd.Name == "sandbox-old" {
				return false
			}
		}
		return true
	})
	if _, err := h.client.CoreV1().Pods(kubeNS).Get(context.Background(), "multica-backend", metav1.GetOptions{}); err != nil {
		t.Fatalf("a pod without the manager's label was touched: %v", err)
	}
}

func TestLoadPodTemplate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.yaml", "apiVersion: v1\nkind: Pod\nspec:\n  containers:\n    - name: main\n      image: pod:1\n")
	if pd, err := sandbox.LoadPodTemplate(good); err != nil || pd.Spec.Containers[0].Image != "pod:1" {
		t.Fatalf("LoadPodTemplate(good) = %v, %v", pd, err)
	}
	for name, body := range map[string]string{
		"typo.yaml":      "kind: Pod\nspec:\n  containerz: []\n",
		"deploy.yaml":    "kind: Deployment\nspec: {}\n",
		"empty.yaml":     "kind: Pod\nspec:\n  containers: []\n",
		"ambiguous.yaml": "kind: Pod\nspec:\n  containers:\n    - {name: a, image: x}\n    - {name: b, image: y}\n",
	} {
		if _, err := sandbox.LoadPodTemplate(write(name, body)); err == nil {
			t.Errorf("LoadPodTemplate(%s) accepted it", name)
		}
	}
}

// A pod's hostname would be its name, which agents print into what reaches
// Multica; a template that sets none gets a neutral one, the same for every
// pod, and one that sets its own keeps it.
func TestKubeGivesPodsANeutralHostname(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 2, MaxPods: 2})
	h.eventually("two warm pods", func() bool { return len(h.pods()) == 2 })
	for _, pd := range h.pods() {
		if pd.Spec.Hostname != "sandbox" {
			t.Errorf("pod %s hostname = %q, want sandbox", pd.Name, pd.Spec.Hostname)
		}
	}

	tpl := kubeTemplate()
	tpl.Spec.Hostname = "agent"
	h = newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 1, Template: tpl})
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	if got := h.pods()[0].Spec.Hostname; got != "agent" {
		t.Errorf("hostname = %q, want the template's agent", got)
	}
}
