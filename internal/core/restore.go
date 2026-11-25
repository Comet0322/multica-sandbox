package core

import (
	"context"

	"sandbox-manager/internal/policy"
)

// Restore takes over what a previous run left in the store: the pods it
// held, which the pool adopts, and the tasks it claimed.
//
//   - A running task runs on.
//   - A task placed on a pod, or handed to it and not reported started, is
//     handed to that pod again.
//   - A task on no pod waits for one.
//   - A hold resumes with its remaining time, and an idle pod ages on.
//   - A pod the pool no longer has is found lost at the next dispatch tick,
//     which fails its started task and places its other task elsewhere.
//   - The failure reports the source had not taken are sent again.
//
// Call it once, before the core serves anything.
func (c *Core) Restore(ctx context.Context) error {
	rec, err := c.store.Load(ctx)
	if err != nil {
		return err
	}
	// The task each running pod runs; a pod holds one at a time.
	running := map[string]bool{}
	for _, p := range rec.Pods {
		if p.Phase == policy.PhaseRunning {
			running[p.ID] = true
		}
	}
	onPod := map[string]*TaskRow{}
	var loose []*TaskRow
	c.mu.Lock()
	for i := range rec.Tasks {
		t := &rec.Tasks[i]
		if t.FailPending {
			c.failing[t.ID] = t
			c.failNow = append(c.failNow, t)
		}
		switch {
		case t.State == TaskEnded:
		case running[t.PodID] && onPod[t.PodID] == nil:
			onPod[t.PodID] = t
		default:
			loose = append(loose, t)
		}
	}
	var adopt []HeldPod
	var kept []PodRow
	for _, p := range rec.Pods {
		if running[p.ID] && onPod[p.ID] == nil {
			// Nothing in the record runs there; whatever does is not the
			// core's to know of.
			c.log.Warn("pod deleted: the dispatch record has no task running on it", "pod_id", p.ID)
			p.GoneAt, p.GoneWhy = c.now(), "the dispatch record has no task running on it"
			c.dirtyPods[p.ID] = p
			continue
		}
		adopt = append(adopt, HeldPod{ID: p.ID, Tenant: p.Tenant})
		kept = append(kept, p)
	}
	c.mu.Unlock()

	if err := c.pool.Adopt(ctx, adopt); err != nil {
		return err
	}

	c.mu.Lock()
	for _, p := range kept {
		c.restorePodLocked(p, onPod[p.ID])
	}
	for _, t := range loose {
		c.restoreLooseLocked(t)
	}
	c.recordLocked()
	c.mu.Unlock()
	c.log.Info("dispatch record restored", "pods", len(kept), "tasks", len(onPod)+len(loose), "failure_reports", c.PendingFailReports())
	// The failure reports go out with the first settle once the source serves
	// its tenants: written now, they would reach none.
	_ = c.flush(ctx)
	return nil
}

// restorePodLocked holds a pod a previous run held, with the task it was
// placed on or handed, if any. Callers hold c.mu.
func (c *Core) restorePodLocked(p PodRow, t *TaskRow) {
	name := p.ID
	if info, ok := c.pool.Pod(p.ID); ok && info.Name != "" {
		name = info.Name
	}
	c.heldSeq++
	pd := &pod{ID: p.ID, Name: name, seq: c.heldSeq, createdAt: p.CreatedAt, readyAt: p.ReadyAt}
	c.held[p.ID] = pd
	c.recorded[p.ID] = p
	info := policy.PodInfo{Phase: p.Phase, Tenant: p.Tenant, Kind: p.Kind, Key: p.Key, Since: p.Since, Until: p.Until}
	if t != nil {
		info.Task, info.Started = t.ID, t.State != TaskPlaced
		if t.State == TaskPlaced {
			c.claimed[t.ID] = t
			pd.queue = []*TaskRow{t}
		} else {
			l := &lease{TaskRow: t, PodName: pd.Name, pod: pd}
			pd.leases = map[string]*lease{t.ID: l}
			c.leases[t.ID] = l
			c.history = append(c.history, l)
		}
	}
	c.applyLocked(c.policy.PodRestored(p.ID, info))
	c.log.Info("pod restored", "pod_id", p.ID, "phase", p.Phase, "workspace_id", p.Tenant, "kind", p.Kind, "task_id", info.Task, "task_state", taskState(t))
}

// restoreLooseLocked takes a task a previous run left on no pod it still
// held: one placed waits for a pod; one its pod was handed is lost with that
// pod. Callers hold c.mu.
func (c *Core) restoreLooseLocked(t *TaskRow) {
	if t.State != TaskPlaced {
		c.log.Warn("restored task's pod is gone; the task is failed", "task_id", t.ID, "pod_id", t.PodID, "workspace_id", t.Tenant)
		c.failLocked(t, "the pod running this task was lost")
		t.State, t.EndedAt, t.EndReason, t.Payload = TaskEnded, c.now(), "pod lost", nil
		c.saveTaskLocked(t)
		return
	}
	t.PodID = ""
	c.claimed[t.ID] = t
	c.saveTaskLocked(t)
	c.applyLocked(c.policy.TaskRestored(t.Task.Task))
	c.log.Info("task restored, waiting for a pod", "task_id", t.ID, "workspace_id", t.Tenant)
}

func taskState(t *TaskRow) TaskState {
	if t == nil {
		return ""
	}
	return t.State
}
