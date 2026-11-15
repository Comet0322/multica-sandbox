package core

import (
	"context"
	"time"

	"sandbox-manager/internal/policy"
)

// The core keeps what each held pod serves on the pod, through its Pool
// (Record), and a core that starts takes over the pods a previous run left
// from those records (Restore). Kubernetes holds the state; the manager
// keeps no database.

// PodRecord is what a held pod serves: the policy's view of it, and when its
// task started.
type PodRecord struct {
	Phase  policy.Phase
	Tenant string
	Kind   policy.Kind
	// Key is the key of its task, or the key it is held for.
	Key string
	// Task is the task placed on or running on the pod; TaskStarted is when
	// the pod picked it up, zero while it has not.
	Task        string
	TaskStarted time.Time
	// Since is when a held pod was held or an idle pod became idle; Until is
	// when a hold ends.
	Since, Until time.Time
}

// RestoredPod is a pod a previous run left, held by the core from now on,
// with what it serves.
type RestoredPod struct {
	ID     string
	Record PodRecord
}

// reasonRestartedBeforeStart fails a task whose pod had not taken it up when
// the manager stopped: the task went with the manager.
const reasonRestartedBeforeStart = "the sandbox manager restarted before the task's pod took it up"

// Restore takes over the pods a previous run left, which the Pool lists as
// held. A running task runs on, its lease restored from when it started; a
// hold resumes with its remaining time; an idle pod ages on. A task placed on
// a pod that had not taken it up is failed, and its pod deleted. Call it
// before the core serves anything.
func (c *Core) Restore(pods []RestoredPod) {
	c.mu.Lock()
	for _, rp := range pods {
		rec := rp.Record
		info, _ := c.pool.Pod(rp.ID)
		c.heldSeq++
		pd := &pod{ID: rp.ID, Name: info.Name, seq: c.heldSeq}
		c.held[rp.ID] = pd
		if rec.Phase == policy.PhaseRunning && rec.TaskStarted.IsZero() {
			c.log.Warn("restored pod had not taken its task up; the task is failed", "pod_id", rp.ID, "task_id", rec.Task, "workspace_id", rec.Tenant)
			c.forced = append(c.forced, forcedTask{tenant: rec.Tenant, id: rec.Task, reason: reasonRestartedBeforeStart})
			c.handBackLocked(policy.Delete{Pod: rp.ID, Reason: "its task was lost in a restart"})
			continue
		}
		c.applyLocked(c.policy.PodRestored(rp.ID, policy.PodInfo{
			Phase: rec.Phase, Tenant: rec.Tenant, Kind: rec.Kind, Key: rec.Key, Task: rec.Task, Since: rec.Since, Until: rec.Until,
		}))
		if rec.Phase == policy.PhaseRunning {
			l := &lease{TaskID: rec.Task, PodID: rp.ID, Tenant: rec.Tenant, StartedAt: rec.TaskStarted, PodName: pd.Name, pod: pd}
			pd.leases = map[string]*lease{rec.Task: l}
			c.leases[rec.Task] = l
			c.history = append(c.history, l)
		}
		c.recorded[rp.ID] = rec
		c.log.Info("pod restored", "pod_id", rp.ID, "phase", rec.Phase, "workspace_id", rec.Tenant, "kind", rec.Kind, "task_id", rec.Task)
	}
	c.recordLocked()
	c.mu.Unlock()
	c.settle(context.Background())
}

// Knows reports whether the core holds a task: leased, or claimed and not
// picked up yet.
func (c *Core) Knows(taskID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leases[taskID] != nil || c.claimed[taskID] != nil
}

// recordLocked tells the Pool what each held pod serves, for those whose
// record changed. Callers hold c.mu.
func (c *Core) recordLocked() {
	for id, pd := range c.held {
		info, ok := c.policy.Pod(id)
		if !ok {
			continue
		}
		rec := PodRecord{Phase: info.Phase, Tenant: info.Tenant, Kind: info.Kind, Key: info.Key, Task: info.Task, Since: info.Since, Until: info.Until}
		if l := pd.leases[info.Task]; l != nil {
			rec.TaskStarted = l.StartedAt
		}
		if old, ok := c.recorded[id]; ok && old == rec {
			continue
		}
		c.recorded[id] = rec
		c.pool.Record(id, rec)
	}
	for id := range c.recorded {
		if c.held[id] == nil {
			delete(c.recorded, id)
		}
	}
}
