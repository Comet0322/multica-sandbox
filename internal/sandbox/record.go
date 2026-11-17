package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// What a pod serves is recorded on the pod itself, so a manager that
// restarts finds it again: Kubernetes holds the state. The
// workspace and kind are labels, the rest annotations.
const (
	WorkspaceLabel        = "sandbox-manager/workspace"
	KindLabel             = "sandbox-manager/kind"
	PhaseAnnotation       = "sandbox-manager/phase"
	KeyAnnotation         = "sandbox-manager/key"
	TaskAnnotation        = "sandbox-manager/task"
	TaskStartedAnnotation = "sandbox-manager/task-started"
	SinceAnnotation       = "sandbox-manager/since"
	UntilAnnotation       = "sandbox-manager/until"
)

// The phases a record names. A pod with no record is warm.
const (
	RecordRunning = "running"
	RecordHeld    = "held"
	RecordIdle    = "idle"
)

// PodRecord is what a pod serves, as the caller tells it with Record. The
// service keeps it on the pod and hands it back with Restore.
type PodRecord struct {
	// Phase is RecordRunning, RecordHeld or RecordIdle; empty (or "warm")
	// for a pod that served no one.
	Phase string
	// Tenant and Kind are the workspace and runtime kind the pod serves.
	Tenant, Kind string
	// Key is the key of its task, or the key it is held for.
	Key string
	// Task is the task placed on or running on the pod, and TaskStarted when
	// the pod picked it up; zero while it has not.
	Task        string
	TaskStarted time.Time
	// Since is when a held pod was held or an idle pod became idle; Until is
	// when a hold ends.
	Since, Until time.Time
}

func (r PodRecord) warm() bool { return r.Phase == "" || r.Phase == "warm" }

// RestoredPod is a pod Restore found serving someone, held by the caller.
type RestoredPod struct {
	ID     PodID
	Record PodRecord
}

// Record keeps what a held pod serves on the pod. It returns at once; the
// pod is updated in the background, always to the latest record. A pod the
// service does not hold is left alone.
func (k *KubeService) Record(id PodID, r PodRecord) {
	defer k.lock()()
	pd := k.pods[id]
	if pd == nil || pd.state != PodHeld || pd.record == r {
		return
	}
	pd.record = r
	k.dirty[id] = true
	select {
	case k.kick <- struct{}{}:
	default:
	}
}

// recordBackoffMax bounds the wait before a record that could not be written
// is tried again; the wait starts at the tick interval and doubles.
const recordBackoffMax = time.Minute

// recordLoop writes the records that changed to their pods until ctx ends. A
// record that could not be written stays to be written and is tried again
// after a backoff, until it is written or its pod is gone.
func (k *KubeService) recordLoop(ctx context.Context) {
	var retry <-chan time.Time
	var backoff time.Duration
	if k.Halted() != "" {
		backoff = k.cfg.TickInterval
		retry = time.After(backoff)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.kick:
			if retry != nil {
				// Backing off: the new record is written with the others.
				continue
			}
		case <-retry:
			retry = nil
		}
		if k.writeRecords(ctx) {
			backoff = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		backoff = min(max(2*backoff, k.cfg.TickInterval), recordBackoffMax)
		retry = time.After(backoff)
	}
}

// writeRecords writes every record that changed; false when one could not be
// written, which then stays to be written.
func (k *KubeService) writeRecords(ctx context.Context) bool {
	for {
		k.mu.Lock()
		var id PodID
		for d := range k.dirty {
			id = d
			break
		}
		if id == "" {
			halted := k.recordRefused != ""
			k.mu.Unlock()
			// With no record left to write, the refusal is checked again
			// on its own.
			return !halted || k.mayRecord(ctx, probePod)
		}
		delete(k.dirty, id)
		pd := k.pods[id]
		if pd == nil || pd.state == PodLost {
			k.mu.Unlock()
			continue
		}
		patch := recordPatch(pd.record)
		k.mu.Unlock()
		c, cancel := context.WithTimeout(ctx, apiTimeout)
		_, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).Patch(c, string(id), types.MergePatchType, patch, metav1.PatchOptions{})
		cancel()
		switch {
		case err == nil:
			k.recordWritten()
			continue
		case ctx.Err() != nil || isNotFound(err):
			continue
		}
		// The pod runs on; a restart would not find what it serves until
		// the record is written.
		why := k.failed("record what pod "+string(id)+" serves (a restart would not keep it)", err)
		unlock := k.lock()
		if cur := k.pods[id]; cur == pd && pd.state != PodLost {
			k.dirty[id] = true
		}
		if refused(err) {
			k.recordRefused = why
		}
		unlock()
		return false
	}
}

// recordWritten notes that a record was written: records are no longer
// refused.
func (k *KubeService) recordWritten() {
	unlock := k.lock()
	k.recordRefused = ""
	unlock()
}

func refused(err error) bool { return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) }

// recordPatch is the merge patch that sets the record's labels and
// annotations and removes those it leaves empty.
func recordPatch(r PodRecord) []byte {
	val := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	tm := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	phase := r.Phase
	if r.warm() {
		phase = ""
	}
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels": map[string]any{WorkspaceLabel: val(r.Tenant), KindLabel: val(r.Kind)},
		"annotations": map[string]any{
			PhaseAnnotation: val(phase), KeyAnnotation: val(r.Key), TaskAnnotation: val(r.Task),
			TaskStartedAnnotation: tm(r.TaskStarted), SinceAnnotation: tm(r.Since), UntilAnnotation: tm(r.Until),
		},
	}})
	return raw
}

// parseRecord reads a pod's record; false for a warm pod with none.
func parseRecord(obj *corev1.Pod) (PodRecord, bool, error) {
	ann := obj.Annotations
	r := PodRecord{
		Phase: ann[PhaseAnnotation], Tenant: obj.Labels[WorkspaceLabel], Kind: obj.Labels[KindLabel],
		Key: ann[KeyAnnotation], Task: ann[TaskAnnotation],
	}
	var errs []error
	tm := func(name string) time.Time {
		v := ann[name]
		if v == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		return t
	}
	r.TaskStarted, r.Since, r.Until = tm(TaskStartedAnnotation), tm(SinceAnnotation), tm(UntilAnnotation)
	if len(errs) > 0 {
		return r, false, errors.Join(errs...)
	}
	if r.warm() && r.Tenant == "" && r.Kind == "" && r.Task == "" {
		return PodRecord{}, false, nil
	}
	switch {
	case r.Tenant == "" || r.Kind == "":
		return r, false, errors.New("no workspace or kind")
	case r.Phase == RecordRunning && r.Task == "":
		return r, false, errors.New("running with no task")
	case r.Phase == RecordHeld && (r.Key == "" || r.Until.IsZero()):
		return r, false, errors.New("held with no key or end")
	case r.Phase == RecordIdle && r.Since.IsZero():
		return r, false, errors.New("idle with no start")
	case r.Phase != RecordRunning && r.Phase != RecordHeld && r.Phase != RecordIdle:
		return r, false, fmt.Errorf("unknown phase %q", r.Phase)
	}
	return r, true, nil
}

// probePod is the pod name the right to patch is checked on when the service
// has no pod: the check is authorized before the pod is looked up.
const probePod = "sandbox-manager-probe"

// mayRecord reports whether the service may write records on its pods: a
// patch of the pod that changes nothing (a server-side dry run) is not
// refused. A refusal halts the service until a record is written.
func (k *KubeService) mayRecord(ctx context.Context, name string) bool {
	c, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	_, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).Patch(c, name, types.MergePatchType, []byte(`{}`),
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	if refused(err) {
		why := k.failed("patch pods to record what they serve", err)
		unlock := k.lock()
		k.recordRefused = why
		unlock()
		return false
	}
	if err == nil || isNotFound(err) {
		k.recordWritten()
	}
	return true
}

// Halted is why no task may be claimed now, empty when tasks may be: while
// Kubernetes refuses the pods' records, a restart would fail every running
// task, so no pod is given out either.
func (k *KubeService) Halted() string {
	defer k.lock()()
	return k.recordRefused
}

// podToken is the pod token in the pod's daemon container.
func podToken(obj *corev1.Pod) string {
	i, err := daemonContainer(obj)
	if err != nil {
		return ""
	}
	for _, e := range obj.Spec.Containers[i].Env {
		if e.Name == PodTicketEnv && strings.HasPrefix(e.Value, podTicketPrefix) {
			return e.Value
		}
	}
	return ""
}

// neverStarted reports whether none of the pod's containers ever started, so
// its daemon cannot have called the manager or run a task.
func neverStarted(obj *corev1.Pod) bool {
	if obj.Status.Phase != corev1.PodPending {
		return false
	}
	for _, cs := range obj.Status.ContainerStatuses {
		if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return false
		}
	}
	return true
}

// Restore lists the pods a previous run left and takes them over: a pod that
// serves someone is held by the caller and returned with its record; a pod
// with no record whose containers never started is kept as warm once its
// daemon calls in; any other pod with no record may run a task whose record
// was never written, and is deleted, as is a pod it cannot interpret (no pod
// token, or a garbled record). It first reads the owner deployment, when one
// is set, and fails when it cannot. Call it once, before Run. A restored pod that ended meanwhile is
// reported lost once Run watches it.
func (k *KubeService) Restore(ctx context.Context) ([]RestoredPod, error) {
	if err := k.resolveOwner(ctx); err != nil {
		return nil, err
	}
	c, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	list, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).List(c, metav1.ListOptions{LabelSelector: k.selector()})
	if err != nil {
		why := k.failed("list the pods", err)
		return nil, errors.New(why)
	}
	items := list.Items
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].CreationTimestamp, items[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return items[i].Name < items[j].Name
	})
	probe := probePod
	if len(items) > 0 {
		probe = items[0].Name
	}
	if !k.mayRecord(ctx, probe) {
		// No record was ever written, so a pod with none may have served
		// anyone: none can be kept.
		for _, it := range items {
			k.log.Warn("deleting a pod a previous run left: what it served is not recorded", "pod", it.Name)
			k.deleteAPI(ctx, it.Name)
		}
		unlock := k.lock()
		k.restored = true
		unlock()
		return nil, nil
	}
	var out []RestoredPod
	var drop []string
	unlock := k.lock()
	k.restored = true
	now := k.now()
	for i := range items {
		obj := &items[i]
		id := PodID(obj.Name)
		ticket := podToken(obj)
		rec, served, err := parseRecord(obj)
		_, ended := podGone(obj, false)
		switch {
		case ticket == "" || k.tickets[ticket] != "" || k.pods[id] != nil:
			k.log.Warn("deleting a pod with no pod token of its own", "pod", id)
			drop = append(drop, obj.Name)
			continue
		case err != nil:
			k.log.Warn("deleting a pod whose record cannot be read", "pod", id, "error", err)
			drop = append(drop, obj.Name)
			continue
		case !served && ended:
			drop = append(drop, obj.Name)
			continue
		case !served && !neverStarted(obj):
			// A task may have been placed on it before its record was
			// written: kept as warm it could serve another key while it
			// still runs that task.
			k.log.Warn("deleting a pod a previous run left: it started and what it serves is not recorded", "pod", id)
			drop = append(drop, obj.Name)
			continue
		}
		k.seq++
		pd := &kubePod{id: id, ticket: ticket, seq: k.seq, createdAt: obj.CreationTimestamp.Time, startBy: now}
		if served {
			pd.state, pd.readyAt, pd.record = PodHeld, now, rec
			out = append(out, RestoredPod{ID: id, Record: rec})
		} else {
			// Its daemon calling in makes it ready, as for a new pod.
			pd.state = PodStarting
		}
		k.pods[id], k.tickets[ticket] = pd, id
	}
	unlock()
	for _, name := range drop {
		k.deleteAPI(ctx, name)
	}
	k.log.Info("pods restored", "serving", len(out), "kept", len(items)-len(drop), "deleted", len(drop))
	return out, nil
}
