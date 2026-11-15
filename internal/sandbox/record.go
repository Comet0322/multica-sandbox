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
// restarts finds it again (ADR 0009: Kubernetes holds the state). The
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
	k.mu.Lock()
	defer k.unlock()
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

// recordLoop writes the records that changed to their pods until ctx ends.
func (k *KubeService) recordLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.kick:
		}
		for {
			k.mu.Lock()
			var id PodID
			for d := range k.dirty {
				id = d
				break
			}
			if id == "" {
				k.mu.Unlock()
				break
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
			if err != nil && ctx.Err() == nil && !isNotFound(err) {
				// The pod runs on; only a restart would not find what it serves.
				k.failed("record what pod "+string(id)+" serves (a restart would not keep it)", err)
			}
		}
	}
}

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

// mayRecord reports whether the service may write records on its pods: a
// patch of the pod that changes nothing (a server-side dry run) is not
// refused.
func (k *KubeService) mayRecord(ctx context.Context, name string) bool {
	c, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	_, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).Patch(c, name, types.MergePatchType, []byte(`{}`),
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		k.failed("patch pods to record what they serve", err)
		return false
	}
	return true
}

// podToken is the pod token in the pod's daemon container.
func podToken(obj *corev1.Pod) string {
	i, err := daemonContainer(obj)
	if err != nil {
		return ""
	}
	for _, e := range obj.Spec.Containers[i].Env {
		if e.Name == PodTokenEnv && strings.HasPrefix(e.Value, podTokenPrefix) {
			return e.Value
		}
	}
	return ""
}

// Restore lists the pods a previous run left and takes them over: a pod that
// serves someone is held by the caller and returned with its record; a pod
// with no record is kept as warm once its daemon calls in; a pod it cannot
// interpret (no pod token, a garbled record, or a warm pod that ended) is
// deleted. Call it once, before Run. A restored pod that ended meanwhile is
// reported lost once Run watches it.
func (k *KubeService) Restore(ctx context.Context) ([]RestoredPod, error) {
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
	if len(items) > 0 && !k.mayRecord(ctx, items[0].Name) {
		// No record was ever written, so a pod with none may have served
		// anyone: none can be kept.
		for _, it := range items {
			k.log.Warn("deleting a pod a previous run left: what it served is not recorded", "pod", it.Name)
			k.deleteAPI(ctx, it.Name)
		}
		k.mu.Lock()
		k.restored = true
		k.unlock()
		return nil, nil
	}
	var out []RestoredPod
	var drop []string
	k.mu.Lock()
	k.restored = true
	now := k.now()
	for i := range items {
		obj := &items[i]
		id := PodID(obj.Name)
		token := podToken(obj)
		rec, served, err := parseRecord(obj)
		_, ended := podGone(obj, false)
		switch {
		case token == "" || k.tokens[token] != "" || k.pods[id] != nil:
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
		}
		k.seq++
		pd := &kubePod{id: id, token: token, seq: k.seq, createdAt: obj.CreationTimestamp.Time, startBy: now}
		if served {
			pd.state, pd.readyAt, pd.record = PodHeld, now, rec
			out = append(out, RestoredPod{ID: id, Record: rec})
		} else {
			// Its daemon calling in makes it ready, as for a new pod.
			pd.state = PodStarting
		}
		k.pods[id], k.tokens[token] = pd, id
	}
	k.unlock()
	for _, name := range drop {
		k.deleteAPI(ctx, name)
	}
	k.log.Info("pods restored", "serving", len(out), "kept", len(items)-len(drop), "deleted", len(drop))
	return out, nil
}
