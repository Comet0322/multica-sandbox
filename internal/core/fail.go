package core

import "context"

// A task decided to fail is reported to the source until the source takes
// the report: at once, then again on every reconciliation pass
// (ReconcileOnce) while the source returns an error.

// failLocked decides to fail a task: its report goes to the source when the
// caller settles. Callers hold c.mu.
func (c *Core) failLocked(tenant, taskID, reason string) {
	t := forcedTask{tenant: tenant, id: taskID, reason: reason}
	c.forced = append(c.forced, t)
	c.failing[taskID] = t
}

// reportFailures sends the failure reports. A report the source took is
// done with; one it did not stays for retryFailures. The reports do not
// depend on ctx being cancelled: it is often a pod's request, which may end
// first.
func (c *Core) reportFailures(ctx context.Context, reports []forcedTask) {
	ctx = context.WithoutCancel(ctx)
	for _, t := range reports {
		if err := c.source.Fail(ctx, t.tenant, t.id, t.reason); err != nil {
			continue
		}
		c.mu.Lock()
		if c.failing[t.id] == t {
			delete(c.failing, t.id)
		}
		c.mu.Unlock()
	}
}

// retryFailures sends again every failure report the source has not taken.
func (c *Core) retryFailures(ctx context.Context) {
	c.mu.Lock()
	reports := make([]forcedTask, 0, len(c.failing))
	for _, t := range c.failing {
		reports = append(reports, t)
	}
	c.mu.Unlock()
	c.reportFailures(ctx, reports)
}

// PendingFailReports counts the failure reports the source has not taken.
func (c *Core) PendingFailReports() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.failing)
}
