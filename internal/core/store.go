package core

import (
	"context"
	"errors"
	"time"

	"sandbox-manager/internal/policy"
)

// The core writes its dispatch record to its Store, the port in ports.go, as
// the record changes. Every change is numbered and leaves its row to write;
// one writer at a time writes every such row and starts at the first change,
// so nothing that changes the record waits for the database. Only what must
// be in the record before the core acts waits for its own change to be
// written: a claim (claimRound), a pod's pickup (Next), and a failure report
// (released by the writer once its row is written). Flush waits for every
// change, as the manager stops.

// storeTimeout bounds one write of the dispatch record.
const storeTimeout = 10 * time.Second

// unwritten is a row to write, with the number of the change that made it.
type unwritten[T any] struct {
	row T
	seq uint64
}

// saveTaskLocked has the task's row written. Callers hold c.mu.
func (c *Core) saveTaskLocked(t *TaskRow) uint64 {
	c.seq++
	c.dirtyTasks[t.ID] = unwritten[TaskRow]{*t, c.seq}
	c.startWriterLocked()
	return c.seq
}

// savePodLocked has a pod's row written. Callers hold c.mu.
func (c *Core) savePodLocked(p PodRow) {
	c.seq++
	c.dirtyPods[p.ID] = unwritten[PodRow]{p, c.seq}
	c.startWriterLocked()
}

// recordLocked has the row of every held pod whose policy view changed
// written. Callers hold c.mu.
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
		c.savePodLocked(row)
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
	c.savePodLocked(row)
}

// startWriterLocked starts the writer unless it runs or nothing is left to
// write. Callers hold c.mu.
func (c *Core) startWriterLocked() {
	if c.writing || len(c.dirtyPods)+len(c.dirtyTasks) == 0 {
		return
	}
	c.writing = true
	go c.write()
}

// write is the writer: it writes every row left to write, one write at a
// time so they land in the order the changes were made, until none is left
// or a write fails. A failed write leaves its rows to the next one, which the
// next change or wait starts.
func (c *Core) write() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.dirtyPods)+len(c.dirtyTasks) > 0 {
		snap := c.seq
		pods := make([]PodRow, 0, len(c.dirtyPods))
		for _, u := range c.dirtyPods {
			pods = append(pods, u.row)
		}
		tasks := make([]TaskRow, 0, len(c.dirtyTasks))
		for _, u := range c.dirtyTasks {
			tasks = append(tasks, u.row)
		}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		err := c.store.Write(ctx, pods, tasks)
		cancel()
		c.mu.Lock()
		c.wroteLocked(snap, err)
		if err != nil {
			break
		}
	}
	c.writing = false
}

// wroteLocked takes the outcome of the write of every change up to snap:
// what it wrote is no longer left to write, the failure reports whose rows
// it wrote go out, and the waiters wake. Callers hold c.mu.
func (c *Core) wroteLocked(snap uint64, err error) {
	defer func() {
		close(c.wrote)
		c.wrote = make(chan struct{})
	}()
	if err != nil {
		c.lastFailed = snap
		if c.storeProblem == "" {
			c.log.Error("write the dispatch record; nothing is claimed or handed to a pod until it is written", "error", err)
		}
		c.storeProblem = err.Error()
		return
	}
	c.written = snap
	for id, u := range c.dirtyPods {
		if u.seq <= snap {
			delete(c.dirtyPods, id)
		}
	}
	for id, u := range c.dirtyTasks {
		if u.seq <= snap {
			delete(c.dirtyTasks, id)
		}
	}
	if c.storeProblem != "" {
		c.log.Info("the dispatch record is written again")
	}
	c.storeProblem = ""
	if reports := c.releaseLocked(true); len(reports) > 0 {
		go c.sendReports(context.Background(), reports)
	}
}

// waitWrittenLocked waits until every change up to seq is written. It
// reports false when a write of that change failed meanwhile, or ctx ended.
// Callers hold c.mu, which it releases while it waits.
func (c *Core) waitWrittenLocked(ctx context.Context, seq uint64) bool {
	for c.written < seq {
		c.startWriterLocked()
		wrote := c.wrote
		c.mu.Unlock()
		select {
		case <-wrote:
		case <-ctx.Done():
		}
		c.mu.Lock()
		if c.written >= seq {
			break
		}
		if ctx.Err() != nil || c.lastFailed >= seq {
			return false
		}
	}
	return true
}

// waitTaskLocked waits until the task's row, as it is now, is written; false
// when a write of it failed meanwhile, or ctx ended. Callers hold c.mu, which
// it releases while it waits.
func (c *Core) waitTaskLocked(ctx context.Context, id string) bool {
	for {
		u, ok := c.dirtyTasks[id]
		if !ok {
			return true
		}
		if !c.waitWrittenLocked(ctx, u.seq) {
			return false
		}
	}
}

// Flush waits until every change made so far is in the store. The manager
// calls it as it stops, so the next run takes over the latest record. It
// returns why it is not written when a write failed, or ctx ended.
func (c *Core) Flush(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitWrittenLocked(ctx, c.seq) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New(c.storeProblem)
}

// StoreProblem is why the dispatch record cannot be written, empty while it
// can. Meanwhile nothing is claimed, and no pod is handed a task.
func (c *Core) StoreProblem() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.storeProblem
}
