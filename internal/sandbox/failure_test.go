package sandbox

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestFailuresAreClassified(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	waiting := func(reason string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "daemon", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "detail"}},
		}}}}
	}
	terminated := func(reason string, code int32) *corev1.Pod {
		pd := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "daemon", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}},
		}}}}
		pd.Spec.Containers = []corev1.Container{{Name: "daemon", LivenessProbe: &corev1.Probe{},
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}}}
		return pd
	}
	blocked := func(pd *corev1.Pod) Failure {
		f, ok := startBlocked(pd)
		if !ok {
			t.Fatalf("startBlocked(%v) = not blocked", pd.Status)
		}
		return f
	}
	gone := func(pd *corev1.Pod, deleted bool) Failure {
		f, ok := podGone(pd, deleted)
		if !ok {
			t.Fatalf("podGone(%v) = not gone", pd.Status)
		}
		return f
	}
	cases := []struct {
		name      string
		f         Failure
		kind      FailureKind
		permanent bool
		what      string
		advice    string
	}{
		{"refused", apiFailure(PhaseCreate, "create pod p", apierrors.NewForbidden(pods, "p", errors.New("no"))),
			FailRefused, true, "Kubernetes refused the manager's credentials", "create, delete, get, list and watch"},
		{"unauthorized", apiFailure(PhaseCreate, "create pod p", apierrors.NewUnauthorized("expired")),
			FailRefused, true, "Kubernetes refused the manager's credentials", "Check the manager's rights"},
		{"rejected", apiFailure(PhaseCreate, "create pod p", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "p", nil)),
			FailInvalidTemplate, true, "Kubernetes rejected the pod", "Fix the pod template"},
		{"api down", apiFailure(PhaseCreate, "create pod p", errors.New("connection refused")),
			FailAPI, false, "create pod p: connection refused", "It is retried"},
		{"image never pulled", blocked(waiting("ErrImageNeverPull")), FailImage, true, "the pod's image cannot be pulled: ErrImageNeverPull: detail", "Check the image"},
		{"image backoff", blocked(waiting("ImagePullBackOff")), FailImage, true, "the pod's image cannot be pulled", "Check the image"},
		{"bad config", blocked(waiting("CreateContainerConfigError")), FailInvalidTemplate, true, "the pod's container cannot be created", "Fix the pod template"},
		{"no node", startTimedOut("p", time.Minute, notScheduled+"0/3 nodes are available"), FailUnschedulable, false,
			"pod p did not start within 1m0s: not scheduled: 0/3 nodes are available", "lower SANDBOX_MAX_PODS"},
		{"slow start", startTimedOut("p", time.Minute, ""), FailStartTimeout, false, "pod p did not start within 1m0s", "events and logs"},
		{"oom", gone(terminated("OOMKilled", 137), false), FailOOM, false, "the pod ran out of memory (limit 2Gi)", "Split the task"},
		{"liveness", gone(terminated("Error", 137), false), FailUnresponsive, false, "the pod's daemon stopped answering", "tell an administrator"},
		{"exited", gone(terminated("Error", 2), false), FailExited, false, "the pod's daemon container exited with code 2 (Error)", "Run the task again"},
		{"evicted", gone(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "disk"}}, false),
			FailEvicted, false, "the node evicted the pod: disk", "short of resources"},
		{"deleted", gone(&corev1.Pod{}, true), FailDeleted, false, "the pod was deleted", "Run the task again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			if f.Kind != tc.kind || f.Permanent() != tc.permanent {
				t.Fatalf("kind %s permanent %v, want %s %v", f.Kind, f.Permanent(), tc.kind, tc.permanent)
			}
			if !strings.Contains(f.What, tc.what) || !strings.Contains(f.Advice, tc.advice) {
				t.Fatalf("what %q advice %q, want %q and %q", f.What, f.Advice, tc.what, tc.advice)
			}
			if got := f.String(); got != f.What+". "+f.Advice {
				t.Fatalf("String = %q", got)
			}
		})
	}
}

func TestWaitingThatMayStillStartIsNotBlocked(t *testing.T) {
	for _, reason := range []string{"ContainerCreating", "PodInitializing", ""} {
		pd := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "daemon", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}}}
		if f, ok := startBlocked(pd); ok {
			t.Fatalf("startBlocked(%q) = %+v, want still starting", reason, f)
		}
	}
}
