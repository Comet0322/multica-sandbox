package core

import "context"

// A task decided to fail is marked so in its row (FailPending) before its
// failure is reported, and reported to the source once that mark is
// written: at once, then again on every reconciliation pass (ReconcileOnce)
// while the source returns an error. A restarted core sends again the
// reports a previous run left pending.

// failLocked decides to fail a task with reason. Callers hold c.mu, and
// settle once they release it.
func (c *Core) failLocked(t *TaskRow, reason string) {
	t.FailReason, t.FailPending = reason, true
	c.saveTaskLocked(t)
	c.failing[t.ID] = t
	c.failNow = append(c.failNow, t)
}

// reportFailures reports the tasks' failures to the source. A report the
// source took is no longer pending, which is written at once. The reports
// do not depend on ctx being cancelled: it is often a pod's request, which
// may end first.
func (c *Core) reportFailures(ctx context.Context, tasks []*TaskRow) {
	if len(tasks) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	defer func() { _ = c.flush(ctx) }()
	for _, t := range tasks {
		c.mu.Lock()
		tenant, id, reason, pending := t.Tenant, t.ID, t.FailReason, t.FailPending
		c.mu.Unlock()
		if !pending || c.source.Fail(ctx, tenant, id, reason) != nil {
			continue
		}
		c.mu.Lock()
		if c.failing[id] == t {
			t.FailPending = false
			c.saveTaskLocked(t)
			delete(c.failing, id)
		}
		c.mu.Unlock()
	}
}

// retryFailures sends again every failure report the source has not taken,
// once the record holds them.
func (c *Core) retryFailures(ctx context.Context) {
	if c.flush(ctx) != nil {
		return
	}
	c.mu.Lock()
	tasks := make([]*TaskRow, 0, len(c.failing))
	for _, t := range c.failing {
		tasks = append(tasks, t)
	}
	c.mu.Unlock()
	c.reportFailures(ctx, tasks)
}

// PendingFailReports counts the failure reports the source has not taken.
func (c *Core) PendingFailReports() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.failing)
}
