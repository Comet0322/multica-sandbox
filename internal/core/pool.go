package core

import (
	"context"

	"sandbox-manager/internal/policy"
)

// What the Pool asks the core about the pods it serves. The core answers from
// its dispatch record: it is the only record of which task runs where.

// Owns reports whether a task is leased to the pod.
func (c *Core) Owns(podID, taskID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.leases[taskID]
	return l != nil && l.PodID == podID && l.pod != nil && c.held[podID] == l.pod
}

// TaskEnded tells the core the pod reported a task's end, and the upstream
// took it. The task's lease ends, provided the task is still the pod's.
func (c *Core) TaskEnded(podID, taskID, reason string) {
	c.mu.Lock()
	l := c.leases[taskID]
	owned := l != nil && l.PodID == podID && l.pod != nil && c.held[podID] == l.pod
	if owned {
		c.endTaskLocked(taskID, reason)
	}
	c.mu.Unlock()
	if owned {
		c.settle(context.Background())
	}
}

// MayServe reports whether a pod may act for a tenant, as far as the core
// knows: a pod it holds may when it holds a task of the tenant (leased or
// placed on it), and may not while it works for another tenant or once it
// served anyone. A held pod that is still warm is as clean as a free one;
// for it, and for a pod the core does not hold, the Pool decides.
func (c *Core) MayServe(podID, tenant string) Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	pd := c.held[podID]
	if pd == nil {
		return NotHeld
	}
	for _, l := range pd.leases {
		if l.Tenant == tenant {
			return Allowed
		}
	}
	for _, t := range pd.queue {
		if t.task.Tenant == tenant {
			return Allowed
		}
	}
	if info, ok := c.policy.Pod(podID); (ok && info.Phase != policy.PhaseWarm) || len(pd.leases) > 0 || len(pd.queue) > 0 {
		return Denied
	}
	return Clean
}
