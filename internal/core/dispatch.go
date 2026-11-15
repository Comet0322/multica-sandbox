package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"sandbox-manager/internal/policy"
)

// The core asks the policy for every placement, claim-selection and
// lifetime decision, and carries the decisions out here. The policy is
// called only under c.mu.

// applyLocked carries out the policy's decisions in order. It returns the
// claim among them, nil when there is none, and whether the policy asked for
// a pod to be acquired. A task placed on a pod is announced to that pod.
// Callers hold c.mu, and settle once they release it.
func (c *Core) applyLocked(ds []policy.Decision) (claim *policy.Claim, acquire bool) {
	for _, d := range ds {
		switch d := d.(type) {
		case policy.Claim:
			claim = &d
		case policy.Acquire:
			acquire = true
		case policy.Release:
			// The reservation held nothing on the pool's side.
		case policy.Place:
			c.placeLocked(d)
		case policy.Delete:
			c.handBackLocked(d)
		case policy.Fail:
			c.forced = append(c.forced, forcedTask{tenant: d.Tenant, id: d.Task, reason: d.Reason})
			if t := c.claimed[d.Task]; t != nil {
				c.auditDenied(d.Tenant, t.task, d.Reason)
			}
			c.dropClaimedLocked(d.Task)
		case policy.EndLease:
			c.dropClaimedLocked(d.Task)
			c.closeLeaseLocked(c.leases[d.Task], d.Reason)
		}
	}
	return claim, acquire
}

// placeLocked queues a claimed task on the pod the policy chose and wakes the
// pod; it picks the task up with its next claim.
func (c *Core) placeLocked(d policy.Place) {
	t := c.claimed[d.Task]
	target := c.held[d.Pod]
	if t == nil || target == nil {
		c.log.Error("policy placed a task the core does not hold", "task_id", d.Task, "pod_id", d.Pod)
		return
	}
	c.unqueueLocked(d.Task)
	target.queue = append(target.queue, t)
	c.log.Info("claimed task assigned to pod", "task_id", d.Task, "pod_id", d.Pod, "workspace_id", d.Tenant)
	c.pool.Wake(d.Pod)
}

// takeQueuedLocked hands the pod the oldest task placed on it and starts its
// lease. Nil when none is placed there.
func (c *Core) takeQueuedLocked(pd *pod) *claimedTask {
	if len(pd.queue) == 0 {
		return nil
	}
	t := pd.queue[0]
	pd.queue = pd.queue[1:]
	delete(c.claimed, t.task.ID)
	c.startLeaseLocked(pd, t.task)
	c.policy.TaskStarted(t.task.ID)
	return t
}

// endTaskLocked tells the policy a task is over and carries out what follows:
// its lease ends, and its pod is handed back when nothing else runs there.
func (c *Core) endTaskLocked(taskID, reason string) {
	c.applyLocked(c.policy.TaskEnded(taskID, reason))
}

// unqueueLocked takes a task off whichever pod it is queued on.
func (c *Core) unqueueLocked(taskID string) {
	for _, pd := range c.held {
		pd.queue = slices.DeleteFunc(pd.queue, func(t *claimedTask) bool { return t.task.ID == taskID })
	}
}

// dropClaimedLocked forgets a claimed task that will not start.
func (c *Core) dropClaimedLocked(taskID string) {
	if _, ok := c.claimed[taskID]; !ok {
		return
	}
	delete(c.claimed, taskID)
	c.unqueueLocked(taskID)
}

// DispatchOnce is the dispatch tick, the only place tasks are claimed. It
// reads the pending lists, forgets held pods the pool no longer lends, wakes
// pods that hold a placed task, and claims at most one task against a
// reservation on a held pod, acquiring one when the policy asks; the pod the
// task is placed on is woken to pick it up.
func (c *Core) DispatchOnce(ctx context.Context) {
	c.readPending(ctx)
	c.mu.Lock()
	c.healHeldLocked()
	c.tickLocked()
	var holding []string
	for _, pd := range c.held {
		if len(pd.queue) > 0 {
			holding = append(holding, pd.ID)
		}
	}
	c.mu.Unlock()
	c.settle(ctx)
	for _, id := range holding {
		c.pool.Wake(id)
	}
	c.claimRound(ctx)
}

// readPending reads every claimable tenant's pending list and hands it to the
// policy, which orders claims by it. A tenant whose list could not be read is
// claimed from as if no list were read.
func (c *Core) readPending(ctx context.Context) {
	for _, id := range c.source.Claimable() {
		for _, kind := range c.cfg.Kinds {
			list, err := c.source.Pending(ctx, id, kind)
			c.mu.Lock()
			if t := c.tenants[id]; t != nil && !t.removed {
				if err != nil {
					c.policy.PendingUnknown(id, kind)
				} else {
					c.policy.PendingChanged(id, kind, list)
				}
			}
			c.mu.Unlock()
			if err != nil {
				c.log.Warn("read pending tasks", "workspace_id", id, "kind", kind, "error", err)
			}
		}
	}
}

// claimRound asks the policy for a reservation on a held pod and claims from
// the tenant it picks, trying the next tenant when a claim returns nothing,
// until a claim returns a task or the policy grants no more reservations.
// When the policy asks for a pod, one is acquired from the pool; once the
// pool has none, the policy may reserve a pod in hold instead. Every claim is
// made against a held pod, so every claimed task has somewhere to run (ADR
// 0001).
func (c *Core) claimRound(ctx context.Context) {
	var tried []policy.Target
	noCapacity := false
	for {
		claimable := c.source.Claimable()
		live, starting := c.livePods()
		c.mu.Lock()
		order := make([]string, 0, len(claimable))
		for _, id := range claimable {
			if t := c.tenants[id]; t != nil && !t.removed {
				order = append(order, id)
			}
		}
		claim, acquire := c.applyLocked(c.policy.RequestClaim(policy.ClaimRequest{
			Tenants: order, Kinds: c.cfg.Kinds, Exclude: tried, NoCapacity: noCapacity,
			LivePods: live, Starting: starting,
		}))
		c.mu.Unlock()
		c.settle(ctx)
		if claim == nil {
			if !acquire {
				return
			}
			if !c.acquire(ctx) {
				noCapacity = true
			}
			continue
		}
		tried = append(tried, policy.Target{Tenant: claim.Tenant, Kind: claim.Kind})
		task, err := c.source.Claim(ctx, claim.Tenant, claim.Kind)
		if err != nil {
			// One tenant's upstream failing must not hold back the others.
			c.log.Warn("claim task", "pod_id", claim.Pod, "workspace_id", claim.Tenant, "error", err)
			c.mu.Lock()
			c.applyLocked(c.policy.ClaimReturned(claim.Reservation, nil, true))
			c.mu.Unlock()
			continue
		}
		if task == nil {
			c.mu.Lock()
			c.applyLocked(c.policy.ClaimReturned(claim.Reservation, nil, false))
			c.mu.Unlock()
			continue
		}
		c.placeClaimed(ctx, *claim, task)
		return
	}
}

// placeClaimed hands a claimed task to the policy, which places it, or fails
// the task when it cannot run here.
func (c *Core) placeClaimed(ctx context.Context, claim policy.Claim, task *Task) {
	if !strings.EqualFold(task.Tenant, claim.Tenant) {
		why := "the managed runtime does not serve this task's workspace"
		c.mu.Lock()
		c.applyLocked(c.policy.ClaimReturned(claim.Reservation, nil, false))
		c.mu.Unlock()
		c.auditDenied(claim.Tenant, task, why)
		c.fail(ctx, claim.Tenant, task.ID, why)
		return
	}
	task.Tenant, task.Kind = claim.Tenant, claim.Kind
	c.mu.Lock()
	c.claimed[task.ID] = &claimedTask{task: task, claimedAt: c.now()}
	c.applyLocked(c.policy.ClaimReturned(claim.Reservation, &task.Task, false))
	c.mu.Unlock()
	c.settle(ctx)
}

// Next hands the polling pod the oldest task placed on it, nil when none is.
// It never claims upstream: only the dispatch tick does, against a pod the
// core holds, so a polling pod with nothing placed on it causes no upstream
// call and tasks stay queued upstream until a pod can take them.
func (c *Core) Next(ctx context.Context, podID string) json.RawMessage {
	c.mu.Lock()
	c.tickLocked()
	var payload json.RawMessage
	if pd := c.held[podID]; pd != nil {
		if t := c.takeQueuedLocked(pd); t != nil {
			payload = t.task.Payload
			c.log.Info("leased pod to task", "pod_id", podID, "task_id", t.task.ID, "workspace_id", t.task.Tenant)
		}
	}
	c.mu.Unlock()
	c.settle(ctx)
	return payload
}
