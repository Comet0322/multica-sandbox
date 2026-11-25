package core

import (
	"context"
	"maps"
	"slices"
	"time"

	"sandbox-manager/internal/policy"
)

// The core writes its dispatch record to its Store, the port in ports.go, as
// the record changes. Every change marks a row to write, and settle writes
// the rows before it acts on the network, one write at a time.

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
