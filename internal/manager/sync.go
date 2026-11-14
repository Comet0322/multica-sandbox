package manager

import (
	"context"
	"errors"
	"time"

	"sandbox-manager/internal/sandbox"
)

// DefaultSyncInterval is how often the manager looks for workspaces its account
// joined or left, the stock daemon's own rhythm.
const DefaultSyncInterval = 30 * time.Second

// Discovery. The manager serves every workspace the bot account
// belongs to, the way a member's daemon does: it lists them, registers
// a local runtime in each, and follows the account as it joins or leaves
// workspaces. Pods learn the same list from the manager, so a new workspace
// needs no configuration anywhere.

// listWorkspaces asks Multica which workspaces the bot account belongs to.
func (p *Manager) listWorkspaces(ctx context.Context) ([]WorkspaceConfig, error) {
	listed, err := p.up.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	// To the server the manager is one machine, whichever workspaces it
	// serves; resolve that identity before any workspace is served.
	if _, err := p.up.Identity(ctx); err != nil {
		return nil, err
	}
	out := make([]WorkspaceConfig, 0, len(listed))
	for _, w := range listed {
		out = append(out, WorkspaceConfig{WorkspaceID: w.ID, Slug: w.Slug, RuntimeName: p.cfg.Name})
	}
	return out, nil
}

// syncRemoveAfter is how many listings in a row must leave a workspace out
// before it is removed. Multica reads the list from a replica that may lag, so
// one listing short of a workspace is not taken as the account leaving it.
const syncRemoveAfter = 2

// SyncStatus is the outcome of the latest workspace listing.
type SyncStatus struct {
	OK    bool      `json:"ok"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// notReady explains why the manager is not ready, or returns "" when it is:
// a listing must have succeeded within three sync
// intervals. That is what turns a revoked or expired PAT, or an unreachable
// Multica, into a visible signal; serving goes on meanwhile.
func (p *Manager) notReady() string {
	return p.ws.notReady(p.now(), p.cfg.SyncInterval)
}

// Sync brings the served workspaces in line with the account's memberships.
// Added ones are claimed from at once; left ones drain as a removed workspace
// does, once syncRemoveAfter listings in a row left them out. A failed listing,
// or one that comes back empty, changes nothing: the account losing every
// workspace at once is far likelier to be an error than a decision.
//
// Each pass also retries what may have failed on the way: the visibility of
// registered runtimes, and the hint that sends pods to register a workspace
// that has no registration yet.
func (p *Manager) Sync(ctx context.Context) (ReloadResult, error) {
	list, err := p.listWorkspaces(ctx)
	if err != nil {
		p.log.Warn("list workspaces; keeping the current ones", "error", err)
		p.ws.recordSync(err, p.now())
		return ReloadResult{}, err
	}
	if len(list) == 0 {
		p.log.Warn("the account belongs to no workspace; keeping the current ones")
		p.ws.recordSync(errors.New("the account belongs to no workspace"), p.now())
		return ReloadResult{}, nil
	}
	list = p.ws.debounce(list)
	p.resolveWorkspaceSettings(ctx, list)
	res, err := p.reload(ctx, list)
	p.ws.recordSync(err, p.now())
	if err == nil && (len(res.Added) > 0 || len(res.Removed) > 0) {
		p.api.NotifyWorkspacesChangedNow()
	} else if p.unregisteredWithPods() {
		// A pod's registration for the workspace failed (Multica was down,
		// say); the stock daemon would look again only in 30 minutes.
		p.api.NotifyWorkspacesChangedLimited()
	}
	if err == nil {
		p.recoverOrphansOnce(ctx)
	}
	p.reconcileVisibility(ctx)
	return res, err
}

// recoverOrphansOnce runs on the first successful Sync. The leases of an
// earlier manager are gone with it and every pod is reset, so whatever Multica
// still shows running on the manager's runtimes will never finish: it is
// failed in every served workspace, which lets Multica retry it, without
// waiting for a pod to register the workspace. The first Sync runs before pods
// are served, so no live task can be hit. Best effort: a failure leaves the
// tasks to Multica's own timeout, or to the recovery a pod's registration runs.
func (p *Manager) recoverOrphansOnce(ctx context.Context) {
	p.mu.Lock()
	if p.orphansRecovered {
		p.mu.Unlock()
		return
	}
	p.orphansRecovered = true
	var todo []*workspace
	for _, ws := range p.ws.served() {
		if !p.workspaceBusyLocked(ws.cfg.WorkspaceID) {
			todo = append(todo, ws)
		}
	}
	p.mu.Unlock()
	for _, ws := range todo {
		ws.rt.RecoverOrphans(ctx)
	}
}

// unregisteredWithPods reports whether some served workspace has no upstream
// registration while live pods are in the pool to register it.
func (p *Manager) unregisteredWithPods() bool {
	live := false
	for _, sp := range p.sandbox.Inventory().Pods {
		if sp.State != sandbox.PodLost {
			live = true
			break
		}
	}
	if !live {
		return false
	}
	for _, ws := range p.ws.served() {
		if !ws.rt.Registered() {
			return true
		}
	}
	return false
}

// reconcileVisibility makes sure every served workspace's registered runtimes
// have the wanted visibility (see multica.Runtimes.EnsureVisibility).
func (p *Manager) reconcileVisibility(ctx context.Context) {
	for _, ws := range p.ws.served() {
		ws.rt.EnsureVisibility(ctx)
	}
}

// SyncLoop runs Sync every SyncInterval, and whenever now fires, until ctx
// ends.
func (p *Manager) SyncLoop(ctx context.Context, now <-chan struct{}) {
	t := time.NewTicker(p.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-now:
		}
		_, _ = p.Sync(ctx)
	}
}
