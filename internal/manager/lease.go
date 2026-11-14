package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sandbox-manager/internal/multica"
	"time"
)

// maxLeaseHistory bounds the closed leases kept for the lease query.
const maxLeaseHistory = 1000

// lease is the period in which one pod is assigned one task.
type lease struct {
	TaskID    string
	PodID     string
	StartedAt time.Time
	EndedAt   time.Time
	EndReason string
	// Initiator and PodName are kept for the audit trail.
	Initiator string
	PodName   string
	// WorkspaceID is the workspace the pod is bound to for this lease.
	WorkspaceID string

	pod *pod
	ws  *workspace
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
func (p *Manager) Leases() []LeaseInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]LeaseInfo, 0, len(p.history))
	for _, l := range p.history {
		info := LeaseInfo{TaskID: l.TaskID, WorkspaceID: l.WorkspaceID, Initiator: l.Initiator, PodID: l.PodID, StartedAt: l.StartedAt, EndReason: l.EndReason}
		if !l.EndedAt.IsZero() {
			ended := l.EndedAt
			info.EndedAt = &ended
		}
		out = append(out, info)
	}
	return out
}

// startLeaseLocked assigns a task to an idle pod. Callers hold p.mu.
func (p *Manager) startLeaseLocked(pd *pod, ws *workspace, taskID, initiator string) *lease {
	l := &lease{TaskID: taskID, PodID: pd.ID, StartedAt: p.now(), Initiator: initiator, PodName: pd.Name, pod: pd, ws: ws, WorkspaceID: ws.cfg.WorkspaceID}
	if pd.leases == nil {
		pd.leases = map[string]*lease{}
	}
	pd.leases[taskID] = l
	p.leases[taskID] = l
	p.history = append(p.history, l)
	p.auditLease(AuditLease, l, "pod leased to task")
	if len(p.history) > maxLeaseHistory {
		keep := p.history[:0]
		drop := len(p.history) - maxLeaseHistory
		for _, old := range p.history {
			if drop > 0 && !old.EndedAt.IsZero() {
				drop--
				continue
			}
			keep = append(keep, old)
		}
		p.history = keep
	}
	return l
}

// closeLeaseLocked records a lease's end, on the lending policy's decision.
// What happens to its pod is the policy's decision too. Callers hold p.mu. A
// nil or already closed lease is ignored.
func (p *Manager) closeLeaseLocked(l *lease, reason string) {
	if l == nil || !l.EndedAt.IsZero() {
		return
	}
	l.EndedAt = p.now()
	l.EndReason = reason
	delete(p.leases, l.TaskID)
	p.auditLease(AuditRelease, l, reason)
	if pd := l.pod; pd != nil && pd.leases[l.TaskID] == l {
		delete(pd.leases, l.TaskID)
	}
}

// failUpstream reports a task the manager claimed as failed, with the reason.
// The report does not depend on ctx being cancelled (it is often a pod's
// request, which may end first), and one that does not get through for a
// reason worth retrying is queued: the reconcile loop sends it again (see
// retryFailures). Without that the task would stay running upstream for good.
func (p *Manager) failUpstream(ctx context.Context, ws *workspace, taskID, reason string) {
	if ws == nil {
		return
	}
	err := ws.rt.Conn().Fail(ctx, taskID, reason)
	if err == nil {
		return
	}
	if !multica.RetryableFail(err) {
		p.log.Warn("fail task upstream: refused; not retried", "task_id", taskID, "error", err)
		return
	}
	p.log.Warn("fail task upstream; queued for retry", "task_id", taskID, "error", err)
	p.mu.Lock()
	p.queueFailLocked(ws, taskID, reason)
	p.mu.Unlock()
}

// servedWorkspaces lists the served workspaces in rotation order.
func (p *Manager) servedWorkspaces() []multica.WorkspaceRef {
	order := p.workspaceList()
	out := make([]multica.WorkspaceRef, 0, len(order))
	for _, ws := range order {
		out = append(out, multica.WorkspaceRef{ID: ws.cfg.WorkspaceID, Name: ws.cfg.RuntimeName})
	}
	return out
}

// servedWorkspace returns the canonical ID of a served workspace.
func (p *Manager) servedWorkspace(id string) (string, bool) {
	ws := p.workspaceByID(id)
	if ws == nil {
		return "", false
	}
	return ws.cfg.WorkspaceID, true
}

// runtimeServed reports whether a served workspace owns the runtime.
func (p *Manager) runtimeServed(runtimeID string) bool {
	return p.workspaceByRuntime(runtimeID) != nil
}

// reposRoute is the route for the pod to read a workspace's repos and
// settings with the workspace's credential, when it may (see mayReadRepos).
func (p *Manager) reposRoute(podID, workspaceID string) (*multica.Route, bool) {
	ws := p.workspaceByID(workspaceID)
	if ws == nil || !p.mayReadRepos(podID, ws) {
		return nil, false
	}
	return &multica.Route{AccountAuth: true}, true
}

// mayReadRepos reports whether the pod may read a workspace's repos and
// settings: it holds a task of that workspace (leased or queued on it), or it
// runs no task and has no binding, is registered for the workspace and is
// clean (free in the sandbox service, or held and idle). A pod working for
// another workspace, a pod between its lease's end and the end of its reset,
// and an unknown pod read nothing.
func (p *Manager) mayReadRepos(podID string, ws *workspace) bool {
	id := ws.cfg.WorkspaceID
	free := p.podFree(podID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if pd := p.held[podID]; pd != nil {
		for _, l := range pd.leases {
			if l.WorkspaceID == id {
				return true
			}
		}
		for _, t := range pd.queue {
			if t.ws.cfg.WorkspaceID == id {
				return true
			}
		}
		if _, bound := p.policy.Binding(podID); bound || len(pd.leases) > 0 || len(pd.queue) > 0 {
			return false
		}
		free = true
	}
	if !free {
		return false
	}
	_, ok := p.daemonWorkspaces[podID][id]
	return ok
}

// claimFor hands the polling pod the oldest task placed on it, nil when none
// is. It never claims upstream: only the dispatch tick does, against a pod
// the manager holds, so a polling pod with nothing placed on it causes no
// upstream call and tasks stay queued in Multica until a pod can take them.
func (p *Manager) claimFor(ctx context.Context, podID string) json.RawMessage {
	p.mu.Lock()
	p.tickLocked()
	var queued json.RawMessage
	if pd := p.held[podID]; pd != nil {
		if t := p.takeQueuedLocked(pd); t != nil {
			queued = t.task.Raw
			p.log.Info("leased pod to task", "pod_id", podID, "task_id", t.task.ID, "workspace_id", t.ws.cfg.WorkspaceID)
		}
	}
	p.mu.Unlock()
	p.settle(ctx)
	return queued
}

// taskRoute is the route for a pod's report on a task to Multica, provided the
// task is leased to that pod. A terminal report (complete, fail, cancel-ack)
// ends the lease once Multica took it. It also ends when Multica refuses it
// with 401, 403 or 404: the PAT lost access, or the task or its runtime is
// gone, so Multica no longer knows the task and nothing will ever end it there.
func (p *Manager) taskRoute(podID, taskID, action, path string) (*multica.Route, bool) {
	p.mu.Lock()
	l := p.leases[taskID]
	owned := l != nil && l.PodID == podID && l.pod != nil && p.held[podID] == l.pod
	p.mu.Unlock()
	if !owned {
		return nil, false
	}
	route := &multica.Route{AccountAuth: true, After: func(status int) {
		if !multica.TerminalAction(action) {
			return
		}
		var reason string
		switch {
		case status >= 200 && status <= 299:
			reason = "pod reported " + action
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
			reason = fmt.Sprintf("upstream refused the pod's %s report (HTTP %d)", action, status)
			p.log.Warn("lease ended: upstream refused a terminal report", "task_id", l.TaskID, "action", action, "status", status)
		default:
			return
		}
		p.mu.Lock()
		if p.leases[l.TaskID] == l {
			p.endTaskLocked(l.TaskID, reason)
		}
		p.mu.Unlock()
		p.settle(context.Background())
	}}
	if multica.TaskTokenPath(path) {
		// The server authorizes these with the task-scoped token the daemon
		// carries; it must reach the server unchanged.
		route.AccountAuth = false
	}
	return route, true
}
