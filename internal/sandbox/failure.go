package sandbox

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// FailurePhase is when a pod failed: while it was being created, while it
// was starting, or once it was ready.
type FailurePhase string

const (
	PhaseCreate FailurePhase = "create"
	PhaseStart  FailurePhase = "start"
	PhaseRun    FailurePhase = "run"
)

// FailureKind is what went wrong with a pod.
type FailureKind string

const (
	// FailOOM: a container ran out of memory.
	FailOOM FailureKind = "oom"
	// FailEvicted: the node evicted the pod, or it stopped by itself.
	FailEvicted FailureKind = "evicted"
	// FailUnresponsive: a liveness probe failed and the container was killed.
	FailUnresponsive FailureKind = "unresponsive"
	// FailDeleted: the pod was deleted by someone other than the service.
	FailDeleted FailureKind = "deleted"
	// FailExited: a container exited on its own.
	FailExited FailureKind = "exited"
	// FailStartTimeout: the pod did not get ready within the start timeout.
	FailStartTimeout FailureKind = "start-timeout"
	// FailUnschedulable: no node could take the pod.
	FailUnschedulable FailureKind = "unschedulable"
	// FailImage: the pod's image cannot be pulled.
	FailImage FailureKind = "image"
	// FailRefused: Kubernetes refused the manager's credentials.
	FailRefused FailureKind = "refused"
	// FailInvalidTemplate: Kubernetes rejected the pod the template makes.
	FailInvalidTemplate FailureKind = "invalid-template"
	// FailAPI: a Kubernetes API call failed for another reason.
	FailAPI FailureKind = "api"
)

// Permanent reports whether a failure of this kind recurs on every pod
// created from the same template with the same credentials, so retrying
// does not help until an operator acts.
func (k FailureKind) Permanent() bool {
	switch k {
	case FailImage, FailRefused, FailInvalidTemplate:
		return true
	}
	return false
}

// Failure is one pod failure: what happened, when, and what to do about it.
type Failure struct {
	At    time.Time    `json:"at"`
	Pod   PodID        `json:"pod,omitempty"`
	Phase FailurePhase `json:"phase"`
	Kind  FailureKind  `json:"kind"`
	// What says what happened, Advice what to do next; both are for the
	// member or operator who reads the failure.
	What   string `json:"what"`
	Advice string `json:"advice,omitempty"`
}

// Permanent reports whether retrying does not help (see FailureKind.Permanent).
func (f Failure) Permanent() bool { return f.Kind.Permanent() }

// String is the failure in one line: what happened, then what to do.
func (f Failure) String() string {
	if f.Advice == "" {
		return f.What
	}
	// Kubernetes' own messages often end with a full stop already.
	return strings.TrimRight(f.What, ". ") + ". " + f.Advice
}

// The next steps the failures name.
const (
	adviceRunAgain   = "Run the task again"
	adviceRunReport  = "Run the task again; if it keeps happening, tell an administrator"
	adviceOOM        = "Split the task into smaller steps, or ask an administrator to raise the pod's memory limit"
	adviceEvicted    = "The node was short of resources; run the task again"
	adviceImage      = "Check the image in the pod template and that the nodes can pull it"
	adviceRefused    = "Check the manager's rights on pods in its namespace: create, delete, get, list and watch"
	adviceInvalid    = "Fix the pod template and restart the manager"
	adviceSchedule   = "Free capacity on the nodes, or lower SANDBOX_MAX_PODS or the pod's requests"
	adviceStartCheck = "Check the pod's events and logs"
	adviceAPI        = "It is retried"
)

// apiFailure classifies a failed Kubernetes API call.
func apiFailure(phase FailurePhase, what string, err error) Failure {
	f := Failure{Phase: phase, What: what + ": " + err.Error()}
	switch {
	case apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err):
		f.Kind, f.Advice = FailRefused, adviceRefused
		f.What = "Kubernetes refused the manager's credentials: " + f.What
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		f.Kind, f.Advice = FailInvalidTemplate, adviceInvalid
		f.What = "Kubernetes rejected the pod: " + f.What
	default:
		f.Kind, f.Advice = FailAPI, adviceAPI
	}
	return f
}

// The container waiting reasons that mean the pod will never start as it is.
var (
	imageWaiting = map[string]bool{
		"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
		"ErrImageNeverPull": true, "RegistryUnavailable": true,
	}
	configWaiting = map[string]bool{"CreateContainerConfigError": true, "CreateContainerError": true}
)

// startBlocked reports why a starting pod will never start as it is (its
// image cannot be pulled, its container cannot be created), so it need not
// wait for the start timeout.
func startBlocked(pd *corev1.Pod) (Failure, bool) {
	for _, cs := range pd.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		msg := w.Reason
		if w.Message != "" {
			msg += ": " + strings.TrimSpace(w.Message)
		}
		switch {
		case imageWaiting[w.Reason]:
			return Failure{Phase: PhaseStart, Kind: FailImage, What: "the pod's image cannot be pulled: " + msg, Advice: adviceImage}, true
		case configWaiting[w.Reason]:
			return Failure{Phase: PhaseStart, Kind: FailInvalidTemplate, What: "the pod's container cannot be created: " + msg, Advice: adviceInvalid}, true
		}
	}
	return Failure{}, false
}

// startTimedOut is the failure of a pod that did not get ready in time,
// with what its status last said.
func startTimedOut(id PodID, timeout time.Duration, waiting string) Failure {
	f := Failure{Phase: PhaseStart, Kind: FailStartTimeout, What: fmt.Sprintf("pod %s did not start within %s", id, timeout), Advice: adviceStartCheck}
	if strings.HasPrefix(waiting, notScheduled) {
		f.Kind, f.Advice = FailUnschedulable, adviceSchedule
	}
	if waiting != "" {
		f.What += ": " + waiting
	}
	return f
}

// podGone reports whether the pod can no longer run anything, and why, in
// words a member can read.
func podGone(pd *corev1.Pod, deleted bool) (Failure, bool) {
	for _, cs := range pd.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil {
			continue
		}
		if t.Reason == "OOMKilled" {
			return Failure{Kind: FailOOM, What: "the pod ran out of memory" + memoryLimit(pd, cs.Name), Advice: adviceOOM}, true
		}
		if t.ExitCode == 137 && pd.DeletionTimestamp == nil && hasLivenessProbe(pd, cs.Name) {
			// With restartPolicy Never a failed liveness probe kills the
			// container and leaves only this behind: killed, not out of
			// memory, and the pod not being deleted.
			who := cs.Name + " container"
			if i, err := daemonContainer(pd); err == nil && pd.Spec.Containers[i].Name == cs.Name {
				who = "daemon"
			}
			return Failure{Kind: FailUnresponsive, What: "the pod's " + who + " stopped answering (its liveness probe failed)", Advice: adviceRunReport}, true
		}
		what := fmt.Sprintf("the pod's %s container exited with code %d", cs.Name, t.ExitCode)
		if t.Reason != "" {
			what += " (" + t.Reason + ")"
		}
		return Failure{Kind: FailExited, What: what, Advice: adviceRunReport}, true
	}
	switch {
	case pd.Status.Phase == corev1.PodFailed || pd.Status.Phase == corev1.PodSucceeded:
		what := "the pod stopped"
		if pd.Status.Reason == "Evicted" {
			what = "the node evicted the pod"
		}
		if pd.Status.Message != "" {
			what += ": " + pd.Status.Message
		}
		return Failure{Kind: FailEvicted, What: what, Advice: adviceEvicted}, true
	case deleted || pd.DeletionTimestamp != nil:
		return Failure{Kind: FailDeleted, What: "the pod was deleted", Advice: adviceRunAgain}, true
	}
	return Failure{}, false
}

// notScheduled starts the waiting reason of a pod no node took.
const notScheduled = "not scheduled: "
