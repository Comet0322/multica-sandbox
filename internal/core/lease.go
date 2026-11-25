package core

import "time"

// maxLeaseHistory bounds the closed leases kept for the lease query.
const maxLeaseHistory = 1000

// lease is the period in which one pod is assigned one task: from when the
// pod is first handed the task until the task ended. It is the task's row,
// with the pod it is on.
type lease struct {
	*TaskRow
	// PodName is kept for the audit trail.
	PodName string

	pod *pod
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
		info := LeaseInfo{TaskID: l.ID, WorkspaceID: l.Tenant, Initiator: l.Email, PodID: l.PodID, StartedAt: l.StartedAt, EndReason: l.EndReason}
		if !l.EndedAt.IsZero() {
			ended := l.EndedAt
			info.EndedAt = &ended
		}
		out = append(out, info)
	}
	return out
}

// startLeaseLocked starts the lease of a task its pod was handed, as the
// record says. Callers hold c.mu.
func (c *Core) startLeaseLocked(pd *pod, t *TaskRow) *lease {
	l := &lease{TaskRow: t, PodName: pd.Name, pod: pd}
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

// closeLeaseLocked records a lease's end, on the policy's decision: its task
// ended. What happens to its pod is the policy's decision too. Callers hold
// c.mu. A nil or already closed lease is ignored.
func (c *Core) closeLeaseLocked(l *lease, reason string) {
	if l == nil || l.State == TaskEnded {
		return
	}
	l.State, l.EndedAt, l.EndReason, l.Payload = TaskEnded, c.now(), reason, nil
	c.saveTaskLocked(l.TaskRow)
	delete(c.leases, l.ID)
	c.auditLease(AuditRelease, l, reason)
	if pd := l.pod; pd != nil && pd.leases[l.ID] == l {
		delete(pd.leases, l.ID)
	}
}

// leaseOfLocked is the latest lease of a task, nil when there is none.
func (c *Core) leaseOfLocked(taskID string) *lease {
	if taskID == "" {
		return nil
	}
	for i := len(c.history) - 1; i >= 0; i-- {
		if c.history[i].ID == taskID {
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

// expireLeasesLocked ends the leases older than LeaseMax and fails their
// tasks upstream: whatever the pod is doing, it must not be held for good.
// Callers hold c.mu.
func (c *Core) expireLeasesLocked() {
	now := c.now()
	var over []*lease
	for _, l := range c.leases {
		if now.Sub(l.StartedAt) > c.cfg.LeaseMax {
			over = append(over, l)
		}
	}
	for _, l := range over {
		c.log.Warn("lease ended: maximum lease time exceeded", "task_id", l.ID, "workspace_id", l.Tenant, "pod_id", l.PodID, "started_at", l.StartedAt)
		c.failLocked(l.TaskRow, "task exceeded the maximum lease time")
		c.endTaskLocked(l.ID, "lease exceeded the maximum lease time")
	}
}

// taskLocked is a task the core holds: placed, or leased and not ended. Nil
// when it holds none by that ID. Callers hold c.mu.
func (c *Core) taskLocked(id string) *TaskRow {
	if t := c.claimed[id]; t != nil {
		return t
	}
	if l := c.leases[id]; l != nil {
		return l.TaskRow
	}
	return nil
}
