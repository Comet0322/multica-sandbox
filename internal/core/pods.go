package core

import (
	"context"
	"errors"
	"slices"
	"sort"

	"sandbox-manager/internal/lending"
)

// The core takes a pod from its Pool (Acquire) when the lending policy asks
// for one, keeps it while it lends it or until the policy hands it back, and
// returns it with HandBack. The Pool tells it when a pod it holds is
// interrupted or lost.

// pod is a pod the core holds: acquired from the pool and not handed back.
type pod struct {
	ID string
	// Name is the label the pod registered with, shown only.
	Name string
	// seq orders the held pods by acquisition.
	seq uint64
	// leases are the tasks the pod runs, by task ID, and queue the claimed
	// tasks the lending policy placed on it that it has not picked up yet.
	leases map[string]*lease
	queue  []*claimedTask
}

// handBack is a pod the policy handed back, with the cause for the audit
// trail.
type handBack struct {
	id  string
	why HandBack
}

// PodInterrupted tells the core a pod it holds restarted by itself: its
// started tasks are gone.
func (c *Core) PodInterrupted(id, why string) {
	c.podGone(id, why, func(pol *lending.Policy) []lending.Decision {
		return pol.PodInterrupted(id, "the pod's daemon restarted", why)
	})
}

// PodLost tells the core a pod it holds went silent.
func (c *Core) PodLost(id, why string) {
	c.podGone(id, why, podLost(id))
}

// podLost is the policy's decision for a held pod that went silent or that
// the pool no longer lends.
func podLost(id string) func(*lending.Policy) []lending.Decision {
	return func(pol *lending.Policy) []lending.Decision {
		return pol.PodLost(id, "the pod running this task was lost", "pod lost")
	}
}

// podGone forgets a held pod the pool no longer lends: the policy fails its
// started tasks and places the others elsewhere.
func (c *Core) podGone(id, why string, tell func(*lending.Policy) []lending.Decision) {
	c.mu.Lock()
	c.podGoneLocked(id, why, tell)
	c.mu.Unlock()
	c.settle(context.Background())
}

func (c *Core) podGoneLocked(id, why string, tell func(*lending.Policy) []lending.Decision) {
	pd := c.held[id]
	if pd == nil {
		return
	}
	c.log.Warn("held pod gone; its tasks are failed or placed elsewhere", "pod_id", id, "name", pd.Name, "reason", why, "tasks", len(pd.leases))
	delete(c.held, id)
	c.applyLocked(tell(c.policy))
}

// PodServes tells the core a pod can serve a tenant: its worker registered
// for it. A pod the core holds may be lent to it from now on.
func (c *Core) PodServes(pod, tenant string) {
	c.mu.Lock()
	c.applyLocked(c.policy.PodRegistered(pod, tenant))
	c.mu.Unlock()
	c.settle(context.Background())
}

// acquire takes a free pod from the pool and records it with the policy, with
// the tenants it can serve. It reports false when the pool has none to give.
// A pod the pool no longer lends by the time it is recorded (a notification
// for it arrived before the core knew it) is not recorded; that counts as an
// acquisition, so the caller tries again.
func (c *Core) acquire(ctx context.Context) bool {
	id, err := c.pool.Acquire(ctx)
	if err != nil {
		if !errors.Is(err, ErrNoCapacity) {
			c.log.Warn("acquire a pod from the pool", "error", err)
		}
		return false
	}
	// The check and the record are one step under c.mu: a notification that
	// comes after it finds the pod held, and one that came before is seen here.
	c.mu.Lock()
	info, ok := c.pool.Pod(id)
	if !ok || info.State != PodHeld {
		c.mu.Unlock()
		c.log.Warn("acquired pod is gone already; not recorded", "pod_id", id)
		return true
	}
	c.heldSeq++
	c.held[id] = &pod{ID: id, Name: info.Name, seq: c.heldSeq}
	tenants := c.pool.Tenants(id)
	c.applyLocked(c.policy.PodAcquired(id, tenants...))
	c.mu.Unlock()
	c.log.Info("pod acquired", "pod_id", id, "name", info.Name, "workspaces", tenants)
	c.settle(ctx)
	return true
}

// handBackLocked forgets a held pod the policy decided to reset and queues it
// to be handed back, with the lease that caused the reset as its audit label.
// Callers hold c.mu, and settle once they release it.
func (c *Core) handBackLocked(d lending.Reset) {
	pd := c.held[d.Pod]
	if pd == nil {
		return
	}
	delete(c.held, d.Pod)
	why := HandBack{Reason: d.Reason}
	if l := c.leaseOfLocked(d.Task); l != nil {
		why.TaskID, why.Tenant, why.Initiator = l.TaskID, l.Tenant, l.Initiator
	}
	c.log.Info("pod handed back to be reset", "pod_id", pd.ID, "reason", d.Reason)
	c.handBacks = append(c.handBacks, handBack{id: pd.ID, why: why})
}

// settle carries out what decisions taken under c.mu left for the network:
// it hands the pods back to the pool and fails upstream the tasks the policy
// decided to fail. Callers invoke it without holding c.mu.
func (c *Core) settle(ctx context.Context) {
	c.mu.Lock()
	backs := c.handBacks
	c.handBacks = nil
	forced := c.forced
	c.forced = nil
	c.mu.Unlock()
	for _, b := range backs {
		if err := c.pool.HandBack(context.WithoutCancel(ctx), b.id, b.why); err != nil {
			c.log.Error("hand a pod back to the pool", "pod_id", b.id, "error", err)
		}
	}
	for _, t := range forced {
		c.fail(ctx, t.tenant, t.id, t.reason)
	}
}

// healHeldLocked forgets the held pods the pool no longer lists as held (a
// notification that could not be delivered, such as one for a pod pruned
// meanwhile). Callers hold c.mu.
func (c *Core) healHeldLocked() {
	for _, id := range c.heldOrderLocked() {
		if info, ok := c.pool.Pod(id); !ok || info.State != PodHeld {
			c.podGoneLocked(id, "the pool no longer lends the pod", podLost(id))
		}
	}
}

// heldOrderLocked lists the held pods to reserve for a claim, best first:
// the preferred ones, then the others, each in acquisition order. Callers
// hold c.mu.
func (c *Core) heldOrderLocked() []string {
	pods := make([]*pod, 0, len(c.held))
	for _, pd := range c.held {
		pods = append(pods, pd)
	}
	slices.SortFunc(pods, func(a, b *pod) int {
		if wa, wb := c.pool.Preferred(a.ID), c.pool.Preferred(b.ID); wa != wb {
			if wa {
				return -1
			}
			return 1
		}
		return int(a.seq) - int(b.seq)
	})
	ids := make([]string, len(pods))
	for i, pd := range pods {
		ids[i] = pd.ID
	}
	return ids
}

// livePods counts the pods the pool can lend: every pod it knows but the
// lost ones. The lending policy scales the guarantees to it.
func (c *Core) livePods() int {
	n := 0
	for _, p := range c.pool.Pods() {
		if p.State != PodLost {
			n++
		}
	}
	return n
}

// initiatorsOf lists the distinct initiators of the tasks the pod holds, sorted.
func initiatorsOf(pd *pod) []string {
	var out []string
	for _, l := range pd.leases {
		if l.Initiator != "" && !slices.Contains(out, l.Initiator) {
			out = append(out, l.Initiator)
		}
	}
	sort.Strings(out)
	return out
}

// firstLease is the pod's longest-held lease, nil when it holds none.
func firstLease(pd *pod) *lease {
	var first *lease
	for _, l := range pd.leases {
		if first == nil || l.StartedAt.Before(first.StartedAt) {
			first = l
		}
	}
	return first
}
