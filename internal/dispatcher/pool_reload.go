package dispatcher

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// DefaultDrainTimeout is how long a workspace removed by a reload may keep its
// leased tasks before they are ended.
const DefaultDrainTimeout = 10 * time.Minute

// normalizeWorkspaces validates a workspace list in place: placement defaults
// to the pool's, runtime name to the pool's name, and IDs must be unique.
func (c *PoolConfig) normalizeWorkspaces(list []WorkspaceConfig) error {
	seen := map[string]bool{}
	for i := range list {
		if list[i].Placement == "" {
			list[i].Placement = c.Placement
		}
		if err := list[i].normalize(c.Name); err != nil {
			return err
		}
		id := list[i].WorkspaceID
		if seen[id] {
			return fmt.Errorf("workspace %s is configured twice", id)
		}
		seen[id] = true
	}
	return nil
}

// workspaceList is a snapshot of the served workspaces in rotation order.
// reload replaces the slice instead of editing it, so the snapshot is safe to
// range over without the lock.
func (p *Pool) workspaceList() []*workspace {
	return p.ws.served()
}

// ReloadResult lists the workspace IDs a reload touched.
type ReloadResult struct {
	Added   []string
	Removed []string
	// Updated had its quota, affinity rule, hold timeout or maximum lifetime
	// changed, which applies to claims and placements from now on.
	Updated []string
	// Unapplied had its runtime name changed, which needs a restart; the
	// running name stays in effect.
	Unapplied []string
}

// reload replaces the served workspaces with the list Sync discovered without
// touching pods or other workspaces' leases.
//
// A new workspace is polled from the next claim on. A removed workspace is no
// longer claimed from at once; its leased tasks run on, and once they have
// ended, or DrainTimeout passed and the leases were ended, its upstream
// connection is closed. The drain runs until ctx ends. An invalid list is
// refused whole and the current configuration stays in effect.
func (p *Pool) reload(ctx context.Context, workspaces []WorkspaceConfig) (ReloadResult, error) {
	var res ReloadResult
	if len(workspaces) == 0 {
		return res, fmt.Errorf("workspace config lists no workspaces")
	}
	next := slices.Clone(workspaces)
	if err := p.cfg.normalizeWorkspaces(next); err != nil {
		return res, err
	}

	p.mu.Lock()
	res, dropped := p.ws.apply(next, func(wc WorkspaceConfig) *workspace {
		return &workspace{cfg: wc, up: p.up.connect(p.cfg.Ticket)}
	})
	for _, ws := range p.ws.served() {
		p.configureWorkspaceLocked(ws)
	}
	gens := make([]int, len(dropped))
	for i, ws := range dropped {
		gens[i] = ws.drainGen
		p.applyLocked(p.policy.RemoveWorkspace(ws.cfg.WorkspaceID))
	}
	p.mu.Unlock()
	p.settle(ctx)

	for i, ws := range dropped {
		go p.drainFrom(ctx, ws, gens[i])
	}
	// Sync reloads every pass; only a pass that changed something is news.
	logAt := p.log.Debug
	if len(res.Added)+len(res.Removed)+len(res.Updated)+len(res.Unapplied) > 0 {
		logAt = p.log.Info
	}
	logAt("workspace configuration reloaded",
		"workspaces", len(next), "added", res.Added, "removed", res.Removed,
		"settings_updated", res.Updated, "restart_needed", res.Unapplied)
	return res, nil
}

// workDoneLocked reports whether a removed workspace holds nothing: no lease,
// no claimed task waiting for a pod, and no claim in flight. Callers hold
// p.mu.
func (p *Pool) workDoneLocked(ws *workspace) bool {
	return p.leasedLocked(ws) == 0 && !p.policy.Busy(ws.cfg.WorkspaceID)
}

// drainFrom waits for the work of a workspace removed as removal number gen to
// end, then disconnects it. It stops without touching
// anything once the workspace is served again or removed anew (gen moved on).
func (p *Pool) drainFrom(ctx context.Context, ws *workspace, gen int) {
	timeout := time.NewTimer(p.cfg.DrainTimeout)
	defer timeout.Stop()
	tick := time.NewTicker(p.cfg.DrainPoll)
	defer tick.Stop()
	for {
		p.mu.Lock()
		current := ws.removed && ws.drainGen == gen
		done := current && p.workDoneLocked(ws)
		p.mu.Unlock()
		if !current {
			p.log.Info("drain stopped: the workspace is served again", "workspace_id", ws.cfg.WorkspaceID)
			return
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-timeout.C:
			p.endDrainingLeases(ctx, ws)
		}
	}
	p.mu.Lock()
	if !ws.removed || ws.drainGen != gen {
		p.mu.Unlock()
		return
	}
	p.ws.finishDrain(ws)
	p.mu.Unlock()
	ws.up.client.CloseIdleConnections()
	p.log.Info("removed workspace drained and disconnected", "workspace_id", ws.cfg.WorkspaceID)
}

// endDrainingLeases ends what is left of a removed workspace's leases when the
// drain timeout passed, and fails those tasks upstream while the connection
// is still open. Claimed tasks still waiting for a pod lapse through the
// placement wait. A workspace that is served again keeps its leases.
func (p *Pool) endDrainingLeases(ctx context.Context, ws *workspace) {
	var ended []*lease
	p.mu.Lock()
	if !ws.removed {
		p.mu.Unlock()
		return
	}
	for _, l := range p.leases {
		if l.ws == ws {
			ended = append(ended, l)
		}
	}
	for _, l := range ended {
		p.endTaskLocked(l.TaskID, "workspace removed from the configuration")
	}
	p.mu.Unlock()
	p.settle(ctx)
	for _, l := range ended {
		p.log.Warn("lease ended: workspace was removed and the drain timeout passed", "task_id", l.TaskID, "workspace_id", ws.cfg.WorkspaceID)
		p.failUpstream(ctx, ws, l.TaskID, "the workspace was removed from the sandbox pool while this task was running")
	}
}
