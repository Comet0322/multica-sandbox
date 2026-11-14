package dispatcher

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"sandbox-manager/internal/lending"
)

// The Dispatcher asks the lending policy (package lending) for every
// placement, claim-selection and lifetime decision, and carries the decisions
// out here. The policy is called only under p.mu.

// newPolicy builds the lending policy for a normalized configuration.
func (p *Pool) newPolicy() error {
	pol, err := lending.New(lending.Config{
		Now:           p.now,
		Capacity:      lending.Resources{Slots: p.cfg.Capacity},
		Rule:          p.cfg.Placement,
		HoldTimeout:   p.cfg.HoldTimeout,
		MaxLifetime:   p.cfg.PodMaxLifetime,
		DrainGrace:    p.cfg.PodDrainGrace,
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
func (p *Pool) configureWorkspaceLocked(ws *workspace) {
	s := lending.WorkspaceSettings{
		Quota: ws.cfg.Quota, Rule: ws.cfg.Placement,
		HoldTimeout: ws.cfg.HoldTimeout, MaxLifetime: ws.cfg.MaxLifetime,
	}
	if err := p.policy.SetWorkspace(ws.cfg.WorkspaceID, s); err != nil {
		// Normalization refuses such settings before they get here.
		p.log.Error("lending policy refused workspace settings", "workspace_id", ws.cfg.WorkspaceID, "error", err)
	}
}

// podBindingLocked describes a pod's binding for the inventory, nil when it
// has none. Callers hold p.mu.
func (p *Pool) podBindingLocked(podID string, now time.Time) *PodBinding {
	b, ok := p.policy.Binding(podID)
	if !ok {
		return nil
	}
	out := &PodBinding{
		ID: uint64(b.ID), AffinityKey: b.Key, WorkspaceID: b.Workspace, Tasks: b.Tasks,
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
func (p *Pool) applyLocked(ds []lending.Decision) (claim *lending.Claim, acquire bool) {
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
			if ws := p.taskWorkspaceLocked(d.Task, d.Workspace); ws != nil {
				p.forced = append(p.forced, forcedTask{ws: ws, id: d.Task, reason: d.Reason})
			}
			if t := p.claimed[d.Task]; t != nil {
				p.auditDenied(t.ws, t.ref, d.Reason)
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
func (p *Pool) placeLocked(d lending.Place) {
	t := p.claimed[d.Task]
	target := p.held[d.Pod]
	if t == nil || target == nil {
		p.log.Error("lending policy placed a task the manager does not hold", "task_id", d.Task, "pod_id", d.Pod)
		return
	}
	p.unqueueLocked(d.Task)
	target.queue = append(target.queue, t)
	p.log.Info("claimed task assigned to pod", "task_id", d.Task, "pod_id", d.Pod, "workspace_id", d.Workspace)
	p.api.wakePod(d.Pod)
}

// takeQueuedLocked hands the pod the oldest task placed on it and starts its
// lease. Nil when none is placed there.
func (p *Pool) takeQueuedLocked(pd *pod) *parkedTask {
	if len(pd.queue) == 0 {
		return nil
	}
	t := pd.queue[0]
	pd.queue = pd.queue[1:]
	delete(p.claimed, t.ref.ID)
	p.startLeaseLocked(pd, t.ws, t.ref.ID, t.ref.initiator())
	p.policy.TaskStarted(t.ref.ID)
	return t
}

// endTaskLocked tells the policy a task is over and carries out what follows:
// its lease ends, and its pod is handed back when nothing else runs there.
func (p *Pool) endTaskLocked(taskID, reason string) {
	p.applyLocked(p.policy.TaskEnded(taskID, reason))
}

// unqueueLocked takes a task off whichever pod it is queued on.
func (p *Pool) unqueueLocked(taskID string) {
	for _, pd := range p.held {
		pd.queue = slices.DeleteFunc(pd.queue, func(t *parkedTask) bool { return t.ref.ID == taskID })
	}
}

// dropClaimedLocked forgets a claimed task that will not start.
func (p *Pool) dropClaimedLocked(taskID string) {
	if _, ok := p.claimed[taskID]; !ok {
		return
	}
	delete(p.claimed, taskID)
	p.unqueueLocked(taskID)
}

// leaseOfLocked is the latest lease of a task, nil when there is none.
func (p *Pool) leaseOfLocked(taskID string) *lease {
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
func (p *Pool) taskWorkspaceLocked(taskID, workspaceID string) *workspace {
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
func (p *Pool) claimTargetsLocked() ([]string, map[string]claimTarget) {
	var order []string
	targets := map[string]claimTarget{}
	for _, ws := range p.ws.served() {
		ids := p.runtimeIDsLocked(ws)
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
func (p *Pool) claimRound(ctx context.Context) {
	var tried []string
	noCapacity := false
	for {
		p.mu.Lock()
		order, targets := p.claimTargetsLocked()
		claim, acquire := p.applyLocked(p.policy.RequestClaim(lending.ClaimRequest{
			Pods: p.heldOrderLocked(), Workspaces: order, Exclude: tried, NoCapacity: noCapacity,
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
		tried = append(tried, claim.Workspace)
		t := targets[claim.Workspace]
		raw, err := p.up.claim(ctx, t.ws.up, p.up.knownIdentity(), t.runtimeIDs)
		p.recordUpstream(t.ws, "claim", err)
		if err != nil {
			// One workspace's upstream failing must not hold back the others.
			p.log.Warn("claim task", "pod_id", claim.Pod, "workspace_id", claim.Workspace, "error", err)
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
func (p *Pool) placeClaimed(ctx context.Context, ws *workspace, claim lending.Claim, raw json.RawMessage) {
	release := func() {
		p.mu.Lock()
		p.applyLocked(p.policy.ClaimReturned(claim.Reservation, nil, false))
		p.mu.Unlock()
	}
	var ref claimedTaskRef
	if err := json.Unmarshal(raw, &ref); err != nil || ref.ID == "" {
		p.log.Error("claimed task has no ID; it will be reclaimed by the server", "pod_id", claim.Pod, "workspace_id", ws.cfg.WorkspaceID)
		release()
		return
	}
	if !strings.EqualFold(ref.WorkspaceID, ws.cfg.WorkspaceID) {
		why := "the managed runtime does not serve this task's workspace"
		release()
		p.auditDenied(ws, ref, why)
		p.failUpstream(ctx, ws, ref.ID, why)
		return
	}

	// A task without a known initiator runs too; the policy gives it a
	// binding of its own.
	task := lending.Task{
		ID: ref.ID, Workspace: ws.cfg.WorkspaceID, Initiator: ref.person(),
		AgentID: ref.AgentID, IssueID: ref.IssueID, ChatSessionID: ref.ChatSessionID,
	}
	p.mu.Lock()
	p.claimed[ref.ID] = &parkedTask{raw: raw, ws: ws, ref: ref, claimedAt: p.now()}
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
func (p *Pool) reportForced(ctx context.Context) {
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
func (p *Pool) readPending(ctx context.Context) {
	type read struct {
		ws         *workspace
		runtimeIDs []string
	}
	var reads []read
	p.mu.Lock()
	for _, ws := range p.ws.served() {
		if ids := p.runtimeIDsLocked(ws); len(ids) > 0 {
			reads = append(reads, read{ws: ws, runtimeIDs: ids})
		}
	}
	p.mu.Unlock()
	for _, r := range reads {
		var list []lending.Pending
		var err error
		for _, id := range r.runtimeIDs {
			var tasks []pendingTask
			if tasks, err = p.up.pending(ctx, r.ws.up, id); err != nil {
				break
			}
			for _, t := range tasks {
				created, _ := time.Parse(time.RFC3339Nano, t.CreatedAt)
				list = append(list, lending.Pending{ID: t.ID, Priority: t.Priority, CreatedAt: created, Status: t.Status})
			}
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
			p.recordUpstream(r.ws, "pending", err)
			p.log.Warn("read pending tasks", "workspace_id", id, "error", err)
		}
	}
}
