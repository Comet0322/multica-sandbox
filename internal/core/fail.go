package core

import (
	"cmp"
	"context"
	"slices"
)

// A task decided to fail is marked so in its row (FailPending), and its
// failure is reported to the source once that mark is written: the writer
// sends it after the write, then every reconciliation pass (ReconcileOnce)
// sends it again while the source returns an error. A report is sent by one
// goroutine at a time. A restarted core sends again the reports a previous
// run left pending, once it knows its tenants.

// report is a failure report the source has not taken.
type report struct {
	row                *TaskRow
	tenant, id, reason string
	// after is the change that marked the row pending: the report goes out
	// only once the record holds it.
	after uint64
	// tried is set once it was sent, and sending while it is being sent.
	tried, sending bool
}

// failLocked decides to fail a task with reason. Callers hold c.mu.
func (c *Core) failLocked(t *TaskRow, reason string) {
	t.FailReason, t.FailPending = reason, true
	seq := c.saveTaskLocked(t)
	c.failing[t.ID] = &report{row: t, tenant: t.Tenant, id: t.ID, reason: reason, after: seq}
}

// releaseLocked claims the reports that may go out now: those whose row is
// written and that no one is sending; with first, only those never sent.
// Callers hold c.mu, and send what it returns.
func (c *Core) releaseLocked(first bool) []*report {
	if !c.tenantsKnown {
		return nil
	}
	var out []*report
	for _, r := range c.failing {
		if r.sending || r.after > c.written || (first && r.tried) {
			continue
		}
		r.sending = true
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *report) int { return cmp.Or(cmp.Compare(a.after, b.after), cmp.Compare(a.id, b.id)) })
	return out
}

// sendReports sends claimed reports to the source. A report the source took
// is no longer pending, which is written. The reports do not depend on ctx
// being cancelled: it is often a pod's request, which may end first.
func (c *Core) sendReports(ctx context.Context, reports []*report) {
	ctx = context.WithoutCancel(ctx)
	for _, r := range reports {
		err := c.source.Fail(ctx, r.tenant, r.id, r.reason)
		c.mu.Lock()
		r.tried, r.sending = true, false
		if err == nil && c.failing[r.id] == r {
			delete(c.failing, r.id)
			r.row.FailPending = false
			c.saveTaskLocked(r.row)
		}
		c.mu.Unlock()
	}
}

// retryFailures sends again every failure report the source has not taken
// and no one is sending.
func (c *Core) retryFailures(ctx context.Context) {
	c.mu.Lock()
	reports := c.releaseLocked(false)
	c.mu.Unlock()
	c.sendReports(ctx, reports)
}

// PendingFailReports counts the failure reports the source has not taken.
func (c *Core) PendingFailReports() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.failing)
}
