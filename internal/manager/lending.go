package manager

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/multica"
)

// The manager asks the lending policy (package lending) for every
// placement, claim-selection and lifetime decision, and carries the decisions
// out here. The policy is called only under p.mu.

// newPolicy builds the lending policy for a normalized configuration.
func (p *Manager) newPolicy() error {
	pol, err := lending.New(lending.Config{
		Now:           p.now,
		Capacity:      lending.Resources{Slots: p.cfg.Capacity},
		Rules:         multica.AffinityRules(),
		Rule:          p.cfg.AffinityRule,
		RemovedReason: "workspace removed",
		HoldTimeout:   p.cfg.HoldTimeout,
		MaxLifetime:   p.cfg.MaxLifetime,
		DrainGrace:    p.cfg.DrainGrace,
		Backoff:       p.cfg.UpstreamBackoff,
		PlacementWait: p.cfg.PlacementWait,
	})
	if err != nil {
		return err
	}
	p.policy = pol
	return nil
}

// configureWorkspaceLocked tells the policy a workspace's settings. Callers
// hold p.mu.
func (p *Manager) configureWorkspaceLocked(ws *workspace) {
	s := lending.TenantSettings{
		Quota: ws.cfg.Quota, Rule: ws.cfg.AffinityRule,
		HoldTimeout: ws.cfg.HoldTimeout, MaxLifetime: ws.cfg.MaxLifetime,
	}
	if err := p.policy.SetTenant(ws.cfg.WorkspaceID, s); err != nil {
		// Normalization refuses such settings before they get here.
		p.log.Error("lending policy refused workspace settings", "workspace_id", ws.cfg.WorkspaceID, "error", err)
	}
}

// podBindingLocked describes a pod's binding for the inventory, nil when it
// has none. Callers hold p.mu.
func (p *Manager) podBindingLocked(podID string, now time.Time) *PodBinding {
	b, ok := p.policy.Binding(podID)
	if !ok {
		return nil
	}
	out := &PodBinding{
		ID: uint64(b.ID), AffinityKey: b.Key, WorkspaceID: b.Tenant, Tasks: b.Tasks,
		UsedSlots: b.Used.Slots, CapacitySlots: p.cfg.Capacity, Hold: b.Hold,
	}
	if b.Hold {
		until := b.HoldUntil
		out.HoldUntil = &until
		out.HoldRemainingSeconds = max(int64(until.Sub(now)/time.Second), 0)
	}
	return out
}

// applyLocked carries out the policy's decisions in order. It returns the
// claim among them, nil when there is none, and whether the policy asked for
// a pod to be acquired. A task placed on a pod is announced to that pod over
// its wake socket. Callers hold p.mu, and settle once they release it.
func (p *Manager) applyLocked(ds []lending.Decision) (claim *lending.Claim, acquire bool) {
	for _, d := range ds {
		switch d := d.(type) {
		case lending.Claim:
			claim = &d
		case lending.Acquire:
			acquire = true
		case lending.Release:
			// The reservation held nothing on the executor's side.
		case lending.Place:
			p.placeLocked(d)
		case lending.Reset:
			p.handBackLocked(d)
		case lending.Fail:
			if ws := p.taskWorkspaceLocked(d.Task, d.Tenant); ws != nil {
				p.forced = append(p.forced, forcedTask{ws: ws, id: d.Task, reason: d.Reason})
			}
			if t := p.claimed[d.Task]; t != nil {
				p.auditDenied(t.ws, t.task, d.Reason)
			}
			p.dropClaimedLocked(d.Task)
		case lending.EndLease:
			p.dropClaimedLocked(d.Task)
			p.closeLeaseLocked(p.leases[d.Task], d.Reason)
		}
	}
	return claim, acquire
}

// placeLocked queues a claimed task on the pod the policy chose and wakes the
// pod; it picks the task up with its next claim.
func (p *Manager) placeLocked(d lending.Place) {
	t := p.claimed[d.Task]
	target := p.held[d.Pod]
	if t == nil || target == nil {
		p.log.Error("lending policy placed a task the manager does not hold", "task_id", d.Task, "pod_id", d.Pod)
		return
	}
	p.unqueueLocked(d.Task)
	target.queue = append(target.queue, t)
	p.log.Info("claimed task assigned to pod", "task_id", d.Task, "pod_id", d.Pod, "workspace_id", d.Tenant)
	p.api.WakePod(d.Pod)
}

// takeQueuedLocked hands the pod the oldest task placed on it and starts its
// lease. Nil when none is placed there.
func (p *Manager) takeQueuedLocked(pd *pod) *claimedTask {
	if len(pd.queue) == 0 {
		return nil
	}
	t := pd.queue[0]
	pd.queue = pd.queue[1:]
	delete(p.claimed, t.task.ID)
	p.startLeaseLocked(pd, t.ws, t.task.ID, t.task.Email())
	p.policy.TaskStarted(t.task.ID)
	return t
}

// endTaskLocked tells the policy a task is over and carries out what follows:
// its lease ends, and its pod is handed back when nothing else runs there.
func (p *Manager) endTaskLocked(taskID, reason string) {
	p.applyLocked(p.policy.TaskEnded(taskID, reason))
}

// unqueueLocked takes a task off whichever pod it is queued on.
func (p *Manager) unqueueLocked(taskID string) {
	for _, pd := range p.held {
		pd.queue = slices.DeleteFunc(pd.queue, func(t *claimedTask) bool { return t.task.ID == taskID })
	}
}

// dropClaimedLocked forgets a claimed task that will not start.
func (p *Manager) dropClaimedLocked(taskID string) {
	if _, ok := p.claimed[taskID]; !ok {
		return
	}
	delete(p.claimed, taskID)
	p.unqueueLocked(taskID)
}

// leaseOfLocked is the latest lease of a task, nil when there is none.
func (p *Manager) leaseOfLocked(taskID string) *lease {
	if taskID == "" {
		return nil
	}
	for i := len(p.history) - 1; i >= 0; i-- {
		if p.history[i].TaskID == taskID {
			return p.history[i]
		}
	}
	return nil
}

// taskWorkspaceLocked is the workspace a task belongs to, served or draining.
func (p *Manager) taskWorkspaceLocked(taskID, workspaceID string) *workspace {
	if t := p.claimed[taskID]; t != nil {
		return t.ws
	}
	if l := p.leases[taskID]; l != nil && l.ws != nil {
		return l.ws
	}
	if ws := p.ws.get(workspaceID); ws != nil {
		return ws
	}
	for _, ws := range p.ws.drainingList() {
		if ws.cfg.WorkspaceID == workspaceID {
			return ws
		}
	}
	return nil
}

// claimTarget is a workspace that may be claimed from, with the runtimes to
// claim on.
type claimTarget struct {
	ws         *workspace
	runtimeIDs []string
}

// claimTargetsLocked lists the served workspaces that have runtimes, in
// rotation order. Callers hold p.mu.
func (p *Manager) claimTargetsLocked() ([]string, map[string]claimTarget) {
	var order []string
	targets := map[string]claimTarget{}
	for _, ws := range p.ws.served() {
		ids := ws.rt.IDs()
		if len(ids) == 0 {
			continue
		}
		order = append(order, ws.cfg.WorkspaceID)
		targets[ws.cfg.WorkspaceID] = claimTarget{ws: ws, runtimeIDs: ids}
	}
	return order, targets
}

// claimRound asks the policy for a reservation on a held pod and claims
// upstream from the workspace it picks, trying the next workspace when a claim
// returns nothing, until a claim returns a task or the policy grants no more
// reservations. When the policy asks for a pod, one is acquired from the
// sandbox service; once the service has none, the policy may reserve a pod in
// hold instead. Every claim is made against a held pod, so every claimed task
// has somewhere to run (ADR 0001).
func (p *Manager) claimRound(ctx context.Context) {
	var tried []string
	noCapacity := false
	for {
		p.mu.Lock()
		order, targets := p.claimTargetsLocked()
		claim, acquire := p.applyLocked(p.policy.RequestClaim(lending.ClaimRequest{
			Pods: p.heldOrderLocked(), Tenants: order, Exclude: tried, NoCapacity: noCapacity,
		}))
		p.mu.Unlock()
		p.settle(ctx)
		if claim == nil {
			if !acquire {
				return
			}
			if !p.acquire(ctx) {
				noCapacity = true
			}
			continue
		}
		tried = append(tried, claim.Tenant)
		t := targets[claim.Tenant]
		raw, err := t.ws.rt.Claim(ctx, t.runtimeIDs)
		if err != nil {
			// One workspace's upstream failing must not hold back the others.
			p.log.Warn("claim task", "pod_id", claim.Pod, "workspace_id", claim.Tenant, "error", err)
			p.mu.Lock()
			p.applyLocked(p.policy.ClaimReturned(claim.Reservation, nil, true))
			p.mu.Unlock()
			continue
		}
		if raw == nil {
			p.mu.Lock()
			p.applyLocked(p.policy.ClaimReturned(claim.Reservation, nil, false))
			p.mu.Unlock()
			continue
		}
		p.placeClaimed(ctx, t.ws, *claim, raw)
		return
	}
}

// placeClaimed hands a claimed task to the policy, which places it, or fails
// the task upstream when it cannot run here.
func (p *Manager) placeClaimed(ctx context.Context, ws *workspace, claim lending.Claim, raw json.RawMessage) {
	release := func() {
		p.mu.Lock()
		p.applyLocked(p.policy.ClaimReturned(claim.Reservation, nil, false))
		p.mu.Unlock()
	}
	claimed, err := multica.ParseClaimed(raw)
	if err != nil {
		p.log.Error("claimed task has no ID; it will be reclaimed by the server", "pod_id", claim.Pod, "workspace_id", ws.cfg.WorkspaceID)
		release()
		return
	}
	if !strings.EqualFold(claimed.WorkspaceID, ws.cfg.WorkspaceID) {
		why := "the managed runtime does not serve this task's workspace"
		release()
		p.auditDenied(ws, claimed, why)
		p.failUpstream(ctx, ws, claimed.ID, why)
		return
	}
	task := claimed.LendingTask(ws.cfg.WorkspaceID)
	p.mu.Lock()
	p.claimed[claimed.ID] = &claimedTask{task: claimed, ws: ws, claimedAt: p.now()}
	p.applyLocked(p.policy.ClaimReturned(claim.Reservation, &task, false))
	p.mu.Unlock()
	p.settle(ctx)
}

// forcedTask is a task the policy decided to fail, not yet failed upstream.
type forcedTask struct {
	ws     *workspace
	id     string
	reason string
}

// reportForced fails upstream the tasks the policy decided to fail.
// Reporting needs the network, so callers invoke it without holding p.mu.
func (p *Manager) reportForced(ctx context.Context) {
	p.mu.Lock()
	ids := p.forced
	p.forced = nil
	p.mu.Unlock()
	for _, t := range ids {
		p.failUpstream(ctx, t.ws, t.id, t.reason)
	}
}

// readPending reads every served workspace's pending list and hands it to the
// policy, which orders claims by it. The server keeps a pending list per
// runtime, so a workspace costs one read per manager runtime there; the
// runtimes' lists are merged. A workspace whose list could not be read is
// claimed from as if no list were read.
func (p *Manager) readPending(ctx context.Context) {
	type read struct {
		ws         *workspace
		runtimeIDs []string
	}
	var reads []read
	p.mu.Lock()
	for _, ws := range p.ws.served() {
		if ids := ws.rt.IDs(); len(ids) > 0 {
			reads = append(reads, read{ws: ws, runtimeIDs: ids})
		}
	}
	p.mu.Unlock()
	for _, r := range reads {
		tasks, err := r.ws.rt.Pending(ctx, r.runtimeIDs)
		var list []lending.Pending
		for _, t := range tasks {
			list = append(list, lending.Pending{ID: t.ID, Priority: t.Priority, CreatedAt: t.Created(), Status: t.Status})
		}
		id := r.ws.cfg.WorkspaceID
		p.mu.Lock()
		if p.ws.get(id) == r.ws {
			if err != nil {
				p.policy.PendingUnknown(id)
			} else {
				p.policy.PendingChanged(id, list)
			}
		}
		p.mu.Unlock()
		if err != nil {
			p.log.Warn("read pending tasks", "workspace_id", id, "error", err)
		}
	}
}
