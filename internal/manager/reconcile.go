package manager

import (
	"context"
	"fmt"
	"net/http"
	"sandbox-manager/internal/multica"
	"slices"
	"sync"
	"time"
)

// Lease reconciliation. A lease ends when its pod reports the task's end, but
// that report can be missed: the daemon acknowledges a cancellation instead of
// reporting, a daemon that restarted forgot its tasks, a report was refused or
// never arrived. Multica knows how each task ended, so the manager asks it
// every ReconcileInterval and ends the leases of tasks that are over. The
// same loop sends again the failure reports that did not get through, and
// LeaseMax bounds every lease whatever happens.

// DefaultReconcileInterval is the reconcile period the command line uses when
// none is set.
const DefaultReconcileInterval = 30 * time.Second

// DefaultLeaseMax bounds a lease when no maximum lease time is set.
const DefaultLeaseMax = 2 * time.Hour

const (
	// reconcileParallel bounds the upstream calls one reconcile pass makes at
	// once.
	reconcileParallel = 8
	// failRetryFor is how long a failure report is retried before it is given
	// up on.
	failRetryFor = 30 * time.Minute
)

// pendingFail is a failure report that did not reach Multica yet.
type pendingFail struct {
	ws     *workspace
	id     string
	reason string
	// since is when the report was first attempted.
	since time.Time
}

// queueFailLocked queues a failure report for the reconcile loop, once per
// task. Callers hold p.mu.
func (p *Manager) queueFailLocked(ws *workspace, taskID, reason string) {
	for _, f := range p.failRetry {
		if f.id == taskID {
			return
		}
	}
	p.failRetry = append(p.failRetry, pendingFail{ws: ws, id: taskID, reason: reason, since: p.now()})
}

// retryFailures sends the queued failure reports again. A report leaves the
// queue once Multica takes it or refuses it (4xx), or after failRetryFor.
func (p *Manager) retryFailures(ctx context.Context) {
	p.mu.Lock()
	queue := p.failRetry
	p.failRetry = nil
	p.mu.Unlock()
	if len(queue) == 0 {
		return
	}
	now := p.now()
	var mu sync.Mutex
	var keep []pendingFail
	parallel(len(queue), reconcileParallel, func(i int) {
		f := queue[i]
		if now.Sub(f.since) > failRetryFor {
			p.log.Error("gave up failing task upstream; it stays open in Multica until its own timeout", "task_id", f.id, "workspace_id", f.ws.cfg.WorkspaceID, "since", f.since)
			return
		}
		err := f.ws.rt.Conn().Fail(ctx, f.id, f.reason)
		switch {
		case err == nil:
			p.log.Info("failed task upstream after retry", "task_id", f.id, "workspace_id", f.ws.cfg.WorkspaceID)
		case multica.RetryableFail(err):
			mu.Lock()
			keep = append(keep, f)
			mu.Unlock()
		default:
			p.log.Warn("fail task upstream: refused on retry; dropped", "task_id", f.id, "error", err)
		}
	})
	p.mu.Lock()
	for _, f := range keep {
		if !slices.ContainsFunc(p.failRetry, func(q pendingFail) bool { return q.id == f.id }) {
			p.failRetry = append(p.failRetry, f)
		}
	}
	p.mu.Unlock()
}

// reconcileLoop runs reconcileOnce every ReconcileInterval until ctx ends.
func (p *Manager) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(p.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce asks Multica for the state of every leased task and ends the
// leases of those that are over, then resends queued failure reports. A task
// that ended upstream (completed, failed, cancelled, or no longer found) is not
// failed again; one whose workspace the PAT lost access to cannot be. Any other
// error keeps the lease for the next pass. No lock is held while calling out.
func (p *Manager) reconcileOnce(ctx context.Context) {
	p.mu.Lock()
	p.tickLocked()
	held := make([]*lease, 0, len(p.leases))
	for _, l := range p.leases {
		if l.ws != nil {
			held = append(held, l)
		}
	}
	p.mu.Unlock()
	p.settle(ctx)

	type verdict struct {
		l      *lease
		reason string
	}
	var mu sync.Mutex
	var ended []verdict
	parallel(len(held), reconcileParallel, func(i int) {
		l := held[i]
		status, err := l.ws.rt.Conn().TaskStatus(ctx, l.TaskID)
		var reason string
		switch code := multica.StatusOf(err); {
		case err == nil && multica.TerminalTaskStatus(status):
			reason = "upstream task ended: " + status
		case code == http.StatusNotFound:
			reason = "upstream task ended: not found"
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			reason = fmt.Sprintf("upstream access lost (HTTP %d)", code)
		case err != nil:
			p.log.Debug("reconcile lease: task status unavailable; kept", "task_id", l.TaskID, "error", err)
			return
		default:
			return
		}
		mu.Lock()
		ended = append(ended, verdict{l, reason})
		mu.Unlock()
	})
	if len(ended) > 0 {
		p.mu.Lock()
		for _, v := range ended {
			if p.leases[v.l.TaskID] != v.l {
				continue // ended meanwhile
			}
			p.log.Warn("lease ended by reconciliation", "task_id", v.l.TaskID, "workspace_id", v.l.WorkspaceID, "pod_id", v.l.PodID, "reason", v.reason)
			p.endTaskLocked(v.l.TaskID, v.reason)
		}
		p.mu.Unlock()
		p.settle(ctx)
	}
	p.retryFailures(ctx)
}

// expireLeasesLocked ends the leases older than LeaseMax and queues their
// tasks to be failed upstream: whatever the pod is doing, it must not be held
// for good. Callers hold p.mu.
func (p *Manager) expireLeasesLocked() {
	if p.cfg.LeaseMax <= 0 {
		return
	}
	now := p.now()
	var over []*lease
	for _, l := range p.leases {
		if now.Sub(l.StartedAt) > p.cfg.LeaseMax {
			over = append(over, l)
		}
	}
	for _, l := range over {
		p.log.Warn("lease ended: maximum lease time exceeded", "task_id", l.TaskID, "workspace_id", l.WorkspaceID, "pod_id", l.PodID, "started_at", l.StartedAt)
		p.forced = append(p.forced, forcedTask{ws: l.ws, id: l.TaskID, reason: "task exceeded the maximum lease time"})
		p.endTaskLocked(l.TaskID, "lease exceeded the maximum lease time")
	}
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
