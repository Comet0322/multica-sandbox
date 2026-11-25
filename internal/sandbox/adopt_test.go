package sandbox_test

import (
	"context"
	"errors"
	"strings"
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

// newAdoptHarness is newKubeHarness without Run: the caller adopts first,
// as the manager does on start.
func newAdoptHarness(t *testing.T, cfg sandbox.KubeConfig, objects ...runtime.Object) *kubeHarness {
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

// leftPod is a pod a previous run created, with its pod token.
func leftPod(name, ticket string) *corev1.Pod {
	l := map[string]string{sandbox.ManagedByLabel: sandbox.ManagedByValue}
	pd := kubeTemplate()
	pd.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: kubeNS, Labels: l,
		CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC))}
	if ticket != "" {
		pd.Spec.Containers[1].Env = []corev1.EnvVar{{Name: sandbox.PodTicketEnv, Value: ticket}}
	} else {
		pd.Spec.Containers[1].Env = nil
	}
	pd.Status.Phase = corev1.PodRunning
	return pd
}

// Adopt holds the pods the caller held, with their pod tokens, and deletes
// every other pod a previous run left, warm ones too, which are made again;
// a pod the caller held that Kubernetes no longer has is not adopted.
func TestKubeAdoptsTheHeldPodsAndDeletesTheRest(t *testing.T) {
	running := leftPod("sandbox-run", "mpt_run")
	held := leftPod("sandbox-held", "mpt_held")
	warm := leftPod("sandbox-warm", "mpt_warm")
	warm.Status.Phase = corev1.PodPending
	noToken := leftPod("sandbox-notoken", "")
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "multica-backend", Namespace: kubeNS}}
	h := newAdoptHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 5}, running, held, warm, noToken, other)

	if err := h.svc.Adopt(context.Background(), []sandbox.PodID{"sandbox-run", "sandbox-held", "sandbox-notoken", "sandbox-gone"}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	for _, name := range []sandbox.PodID{"sandbox-run", "sandbox-held"} {
		if info, ok := h.svc.Pod(name); !ok || info.State != sandbox.PodHeld {
			t.Errorf("pod %s = %+v, want held by the caller", name, info)
		}
	}
	if id, ok := h.svc.Authenticate("mpt_run"); !ok || id != "sandbox-run" {
		t.Errorf("the running pod's token = %q, %v", id, ok)
	}
	for _, name := range []sandbox.PodID{"sandbox-gone", "sandbox-warm", "sandbox-notoken"} {
		if info, ok := h.svc.Pod(name); ok {
			t.Errorf("pod %s = %+v, want unknown", name, info)
		}
	}
	for _, name := range []string{"sandbox-warm", "sandbox-notoken"} {
		if _, ok := h.pod(name); ok {
			t.Errorf("pod %s the caller did not hold was kept", name)
		}
	}
	if _, ok := h.svc.Authenticate("mpt_warm"); ok {
		t.Error("the deleted warm pod's token is still accepted")
	}
	if _, ok := h.pod("multica-backend"); !ok {
		t.Error("a pod the manager did not create was deleted")
	}

	h.run()
	h.eventually("a new warm pod", func() bool { return len(h.pods()) == 4 })
}

// Run with no Adopt before it deletes every pod a previous run left: no
// caller holds them.
func TestKubeRunWithoutAdoptDeletesWhatAPreviousRunLeft(t *testing.T) {
	h := newAdoptHarness(t, sandbox.KubeConfig{MaxPods: 2}, leftPod("sandbox-old", "mpt_old"))
	h.run()
	h.eventually("the old pod deleted", func() bool { _, ok := h.pod("sandbox-old"); return !ok })
}

func TestKubeAdoptedPodThatEndedMeanwhileIsLost(t *testing.T) {
	gone := leftPod("sandbox-oom", "mpt_oom")
	gone.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "daemon", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}}
	h := newAdoptHarness(t, sandbox.KubeConfig{MaxPods: 2}, gone)
	if err := h.svc.Adopt(context.Background(), []sandbox.PodID{"sandbox-oom"}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	h.run()
	h.eventually("the loss noted", func() bool { return len(h.notes.all()) == 1 })
	if n := h.notes.all()[0]; n.id != "sandbox-oom" || !strings.HasPrefix(n.why, "the pod ran out of memory (limit 4Gi). ") {
		t.Fatalf("lost = %+v", n)
	}
}

func TestKubeAdoptNamesRefusedCredentials(t *testing.T) {
	h := newAdoptHarness(t, sandbox.KubeConfig{})
	h.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no list"))
	})
	if err := h.svc.Adopt(context.Background(), nil); err == nil || !contains(err.Error(), "credentials") {
		t.Fatalf("Adopt = %v, want an error naming the credentials", err)
	}
}

func newFakeClient(objects ...runtime.Object) *fake.Clientset {
	return fake.NewSimpleClientset(objects...)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
