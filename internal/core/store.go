package core

import (
	"context"
	"maps"
	"slices"
	"time"

	"sandbox-manager/internal/policy"
)

// Store keeps the core's dispatch record, so a core that starts takes over
// what a previous run left: the pods it held and the tasks it claimed. The
// core writes a row whenever what it holds changes; each row replaces the
// earlier row of the same ID.
type Store interface {
	// Load reads the record a previous run left: the pods it held and did
	// not let go, in the order it acquired them, and the tasks that did not
	// end or whose failure report the source has not taken, in the order
	// they were claimed.
	Load(ctx context.Context) (Record, error)
	// Write saves the rows, each replacing the row of the same ID: all of
	// them, or none when it returns an error.
	Write(ctx context.Context, pods []PodRow, tasks []TaskRow) error
}

// Record is the dispatch record as a Store loads it.
type Record struct {
	Pods  []PodRow
	Tasks []TaskRow
}

// PodRow is a pod the core holds, or held: acquired from the Pool and not
// handed back.
type PodRow struct {
	ID string
	// Phase, Tenant, Kind, Key, Since and Until are what the policy knows of
	// the pod (policy.PodInfo).
	Phase        policy.Phase
	Tenant       string
	Kind         policy.Kind
	Key          string
	Since, Until time.Time
	// CreatedAt is when the pod was created, zero when the Pool did not
	// say; ReadyAt is when the core acquired it, ready.
	CreatedAt, ReadyAt time.Time
	// GoneAt is when the core let the pod go, handed back or lost, and
	// GoneWhy why; zero while the core holds it.
	GoneAt  time.Time
	GoneWhy string
}

// TaskState is where a claimed task stands.
type TaskState string

// The states of a claimed task, in the order it goes through them.
const (
	// TaskPlaced: claimed, and placed on a pod or waiting for one; no pod
	// has been handed it.
	TaskPlaced TaskState = "placed"
	// TaskHanded: handed to its pod's daemon, which has not reported it
	// started. The pod is handed it again until it does.
	TaskHanded TaskState = "handed"
	// TaskRunning: its pod reported it started.
	TaskRunning TaskState = "running"
	// TaskEnded: over, whatever the outcome.
	TaskEnded TaskState = "ended"
)

// TaskRow is a task the core claimed. A Store keeps its Payload, encrypted,
// only while the task is placed or handed, and never keeps its Email.
type TaskRow struct {
	Task
	// PodID is the pod the task is placed on, handed to or runs on; empty
	// while it waits for a pod.
	PodID string
	State TaskState
	// ClaimedAt is when the task was claimed, StartedAt when its pod was
	// first handed it, EndedAt when it ended and EndReason why.
	ClaimedAt, StartedAt, EndedAt time.Time
	EndReason                     string
	// FailReason is the failure the core reports to the source;
	// FailPending is set until the source took the report.
	FailReason  string
	FailPending bool
}

// storeTimeout bounds one write of the dispatch record.
const storeTimeout = 10 * time.Second

// saveTaskLocked has the task's row written with the next flush. Callers
// hold c.mu.
func (c *Core) saveTaskLocked(t *TaskRow) { c.dirtyTasks[t.ID] = *t }

// recordLocked has the row of every held pod whose policy view changed
// written with the next flush. Callers hold c.mu.
func (c *Core) recordLocked() {
	for id, pd := range c.held {
		info, ok := c.policy.Pod(id)
		if !ok {
			continue
		}
		row := PodRow{ID: id, Phase: info.Phase, Tenant: info.Tenant, Kind: info.Kind, Key: info.Key,
			Since: info.Since, Until: info.Until, CreatedAt: pd.createdAt, ReadyAt: pd.readyAt}
		if old, ok := c.recorded[id]; ok && old == row {
			continue
		}
		c.recorded[id] = row
		c.dirtyPods[id] = row
	}
}

// goneLocked has the row of a pod the core lets go written as gone, with
// why. Callers hold c.mu.
func (c *Core) goneLocked(pd *pod, why string) {
	row, ok := c.recorded[pd.ID]
	if !ok {
		row = PodRow{ID: pd.ID, Phase: policy.PhaseWarm, CreatedAt: pd.createdAt, ReadyAt: pd.readyAt}
	}
	row.GoneAt, row.GoneWhy = c.now(), why
	delete(c.recorded, pd.ID)
	c.dirtyPods[pd.ID] = row
}

// flush writes the rows changed since the last write. Writes are one at a
// time, so they land in the order the changes were made; a write that fails
// leaves its rows to the next one, unless newer ones replaced them. A nil
// error means everything changed before the call is written.
func (c *Core) flush(ctx context.Context) error {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.mu.Lock()
	pods, tasks := c.dirtyPods, c.dirtyTasks
	if len(pods)+len(tasks) == 0 {
		c.mu.Unlock()
		return nil
	}
	c.dirtyPods, c.dirtyTasks = map[string]PodRow{}, map[string]TaskRow{}
	c.mu.Unlock()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	err := c.store.Write(wctx, slices.Collect(maps.Values(pods)), slices.Collect(maps.Values(tasks)))
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		if c.storeProblem != "" {
			c.log.Info("the dispatch record is written again")
		}
		c.storeProblem = ""
		return nil
	}
	for id, row := range pods {
		if _, newer := c.dirtyPods[id]; !newer {
			c.dirtyPods[id] = row
		}
	}
	for id, row := range tasks {
		if _, newer := c.dirtyTasks[id]; !newer {
			c.dirtyTasks[id] = row
		}
	}
	if c.storeProblem == "" {
		c.log.Error("write the dispatch record; nothing is claimed or handed to a pod until it is written", "error", err)
	}
	c.storeProblem = err.Error()
	return err
}

// StoreProblem is why the dispatch record cannot be written, empty while it
// can. Meanwhile nothing is claimed, and no pod is handed a task.
func (c *Core) StoreProblem() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.storeProblem
}
