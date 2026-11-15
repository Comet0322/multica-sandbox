package core

import (
	"context"
	"time"
)

// maxLeaseHistory bounds the closed leases kept for the lease query.
const maxLeaseHistory = 1000

// lease is the period in which one pod is assigned one task.
type lease struct {
	TaskID    string
	PodID     string
	Tenant    string
	StartedAt time.Time
	EndedAt   time.Time
	EndReason string
	// Initiator and PodName are kept for the audit trail.
	Initiator string
	PodName   string

	pod *pod
}

// claimedTask is a task claimed upstream that its pod has not picked up yet.
// It sits in its pod's queue once the policy placed it.
type claimedTask struct {
	task      *Task
	claimedAt time.Time
}

// forcedTask is a task the policy decided to fail, not yet failed upstream.
type forcedTask struct {
	tenant string
	id     string
	reason string
}

// LeaseInfo is one lease in the lease query. EndedAt is nil while the lease is
// active.
type LeaseInfo struct {
	TaskID      string `json:"task_id"`
	WorkspaceID string `json:"workspace_id"`
	// Initiator is the email of the person the task runs for.
	Initiator string     `json:"initiator,omitempty"`
	PodID     string     `json:"pod_id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

// Leases lists active and recently closed leases, oldest first.
func (c *Core) Leases() []LeaseInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]LeaseInfo, 0, len(c.history))
	for _, l := range c.history {
		info := LeaseInfo{TaskID: l.TaskID, WorkspaceID: l.Tenant, Initiator: l.Initiator, PodID: l.PodID, StartedAt: l.StartedAt, EndReason: l.EndReason}
		if !l.EndedAt.IsZero() {
			ended := l.EndedAt
			info.EndedAt = &ended
		}
		out = append(out, info)
	}
	return out
}

// startLeaseLocked assigns a task to a held pod. Callers hold c.mu.
func (c *Core) startLeaseLocked(pd *pod, t *Task) *lease {
	l := &lease{TaskID: t.ID, PodID: pd.ID, Tenant: t.Tenant, StartedAt: c.now(), Initiator: t.Email, PodName: pd.Name, pod: pd}
	if pd.leases == nil {
		pd.leases = map[string]*lease{}
	}
	pd.leases[t.ID] = l
	c.leases[t.ID] = l
	c.history = append(c.history, l)
	c.auditLease(AuditLease, l, "pod leased to task")
	if len(c.history) > maxLeaseHistory {
		keep := c.history[:0]
		drop := len(c.history) - maxLeaseHistory
		for _, old := range c.history {
			if drop > 0 && !old.EndedAt.IsZero() {
				drop--
				continue
			}
			keep = append(keep, old)
		}
		c.history = keep
	}
	return l
}

// closeLeaseLocked records a lease's end, on the lending policy's decision.
// What happens to its pod is the policy's decision too. Callers hold c.mu. A
// nil or already closed lease is ignored.
func (c *Core) closeLeaseLocked(l *lease, reason string) {
	if l == nil || !l.EndedAt.IsZero() {
		return
	}
	l.EndedAt = c.now()
	l.EndReason = reason
	delete(c.leases, l.TaskID)
	c.auditLease(AuditRelease, l, reason)
	if pd := l.pod; pd != nil && pd.leases[l.TaskID] == l {
		delete(pd.leases, l.TaskID)
	}
}

// leaseOfLocked is the latest lease of a task, nil when there is none.
func (c *Core) leaseOfLocked(taskID string) *lease {
	if taskID == "" {
		return nil
	}
	for i := len(c.history) - 1; i >= 0; i-- {
		if c.history[i].TaskID == taskID {
			return c.history[i]
		}
	}
	return nil
}

// leasedLocked counts a tenant's active leases. Callers hold c.mu.
func (c *Core) leasedLocked(tenant string) int {
	n := 0
	for _, l := range c.leases {
		if l.Tenant == tenant {
			n++
		}
	}
	return n
}

// expireLeasesLocked ends the leases older than LeaseMax and queues their
// tasks to be failed upstream: whatever the pod is doing, it must not be held
// for good. Callers hold c.mu.
func (c *Core) expireLeasesLocked() {
	now := c.now()
	var over []*lease
	for _, l := range c.leases {
		if now.Sub(l.StartedAt) > c.cfg.LeaseMax {
			over = append(over, l)
		}
	}
	for _, l := range over {
		c.log.Warn("lease ended: maximum lease time exceeded", "task_id", l.TaskID, "workspace_id", l.Tenant, "pod_id", l.PodID, "started_at", l.StartedAt)
		c.forced = append(c.forced, forcedTask{tenant: l.Tenant, id: l.TaskID, reason: "task exceeded the maximum lease time"})
		c.endTaskLocked(l.TaskID, "lease exceeded the maximum lease time")
	}
}

// fail reports a task as failed to the source, which retries what does not
// get through. The report does not depend on ctx being cancelled: it is
// often a pod's request, which may end first.
func (c *Core) fail(ctx context.Context, tenant, taskID, reason string) {
	c.source.Fail(context.WithoutCancel(ctx), tenant, taskID, reason)
}
