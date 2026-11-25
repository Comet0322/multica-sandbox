package core

import (
	"context"
	"sync"
	"time"
)

// Lease reconciliation. A lease ends when its pod reports the task's end, but
// that report can be missed: the daemon acknowledges a cancellation instead of
// reporting, a daemon that restarted forgot its tasks, a report was refused or
// never arrived. The source knows how each task ended, so the core asks it
// every ReconcileInterval and ends the leases of tasks that are over.
// LeaseMax bounds every lease whatever happens.

// reconcileParallel bounds the source calls one reconcile pass makes at once.
const reconcileParallel = 8

// reconcileLoop runs ReconcileOnce every ReconcileInterval until ctx ends.
func (c *Core) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.ReconcileOnce(ctx)
		}
	}
}

// ReconcileOnce asks the source whether each leased task is over and ends the
// leases of those that are, and sends again the failure reports the source
// has not taken. No lock is held while calling out.
func (c *Core) ReconcileOnce(ctx context.Context) {
	c.retryFailures(ctx)
	c.mu.Lock()
	c.tickLocked()
	held := make([]*lease, 0, len(c.leases))
	for _, l := range c.leases {
		held = append(held, l)
	}
	c.mu.Unlock()
	c.settle(ctx)

	type verdict struct {
		l      *lease
		reason string
	}
	var mu sync.Mutex
	var ended []verdict
	parallel(len(held), reconcileParallel, func(i int) {
		l := held[i]
		reason, over := c.source.TaskEnded(ctx, l.Tenant, l.TaskID)
		if !over {
			return
		}
		mu.Lock()
		ended = append(ended, verdict{l, reason})
		mu.Unlock()
	})
	if len(ended) == 0 {
		return
	}
	c.mu.Lock()
	for _, v := range ended {
		if c.leases[v.l.TaskID] != v.l {
			continue // ended meanwhile
		}
		c.log.Warn("lease ended by reconciliation", "task_id", v.l.TaskID, "workspace_id", v.l.Tenant, "pod_id", v.l.PodID, "reason", v.reason)
		c.endTaskLocked(v.l.TaskID, v.reason)
	}
	c.mu.Unlock()
	c.settle(ctx)
}

// parallel runs fn(0..n-1) with at most limit calls at once and waits for all.
func parallel(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			fn(i)
		}()
	}
	wg.Wait()
}
