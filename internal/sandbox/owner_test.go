package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"sandbox-manager/internal/sandbox"
)

func ownerDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-manager", Namespace: kubeNS, UID: types.UID("dep-uid-1")}}
}

func TestNewPodsAreOwnedByTheOwnerDeployment(t *testing.T) {
	h := newRestoreHarness(t, sandbox.KubeConfig{WarmPods: 1, OwnerDeployment: "sandbox-manager"}, ownerDeployment())
	if _, err := h.svc.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.run()
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	refs := h.pods()[0].OwnerReferences
	if len(refs) != 1 {
		t.Fatalf("owner references = %+v, want the deployment", refs)
	}
	r := refs[0]
	if r.APIVersion != "apps/v1" || r.Kind != "Deployment" || r.Name != "sandbox-manager" || r.UID != "dep-uid-1" {
		t.Fatalf("owner = %+v, want deployment sandbox-manager dep-uid-1", r)
	}
	// Not blocking deletion needs no right on the deployment's finalizers;
	// not the controller, since the Deployment controller does not manage
	// these pods.
	if r.BlockOwnerDeletion == nil || *r.BlockOwnerDeletion || r.Controller == nil || *r.Controller {
		t.Fatalf("owner = %+v, want neither blocking deletion nor controller", r)
	}
}

func TestPodsHaveNoOwnerWithoutOne(t *testing.T) {
	h := newKubeHarness(t, sandbox.KubeConfig{WarmPods: 1})
	h.eventually("a warm pod", func() bool { return len(h.pods()) == 1 })
	if refs := h.pods()[0].OwnerReferences; len(refs) != 0 {
		t.Fatalf("owner references = %+v, want none", refs)
	}
}

func TestAnOwnerThatCannotBeReadStopsTheStart(t *testing.T) {
	cases := []struct {
		name    string
		objects []runtime.Object
		refuse  bool
		want    string
	}{
		{"missing", nil, false, "no deployment sandbox-manager in namespace " + kubeNS},
		{"refused", []runtime.Object{ownerDeployment()}, true, "the manager needs get on it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestoreHarness(t, sandbox.KubeConfig{WarmPods: 1, OwnerDeployment: "sandbox-manager"}, tc.objects...)
			if tc.refuse {
				h.client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "sandbox-manager", errors.New("no rights"))
				})
			}
			_, err := h.svc.Restore(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Restore = %v, want %q", err, tc.want)
			}
			if n := len(h.pods()); n != 0 {
				t.Fatalf("pods = %d, want none created", n)
			}
		})
	}
}

// A pod a previous run created before the owner was set keeps running and
// is restored as it is; new pods get the owner.
func TestRestoredPodsWithoutAnOwnerAreKept(t *testing.T) {
	left := leftPod("sandbox-old", "mpt_old", map[string]string{sandbox.WorkspaceLabel: "ws-1", sandbox.KindLabel: "pool"},
		map[string]string{sandbox.PhaseAnnotation: "running", sandbox.TaskAnnotation: "task-1", sandbox.TaskStartedAnnotation: "2026-11-16T08:30:00Z"})
	h := newRestoreHarness(t, sandbox.KubeConfig{WarmPods: 1, MaxPods: 3, OwnerDeployment: "sandbox-manager"}, ownerDeployment(), left)
	got, err := h.svc.Restore(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "sandbox-old" {
		t.Fatalf("Restore = %+v, %v; want the old pod", got, err)
	}
	h.run()
	h.eventually("a new warm pod", func() bool { return len(h.pods()) == 2 })
	for _, pd := range h.pods() {
		owned := len(pd.OwnerReferences) == 1
		if pd.Name == "sandbox-old" && owned {
			t.Fatalf("the restored pod got an owner: %+v", pd.OwnerReferences)
		}
		if pd.Name != "sandbox-old" && !owned {
			t.Fatalf("the new pod %s has no owner", pd.Name)
		}
	}
}
