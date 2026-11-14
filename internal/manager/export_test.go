package manager

import (
	"context"
	"log/slog"
	"time"

	"sandbox-manager/internal/sandbox"
)

// DispatchOnce runs one active-dispatch round, as Run does on each tick.
func (p *Manager) DispatchOnce(ctx context.Context) { p.dispatchOnce(ctx) }

// ReconcileOnce runs one lease reconciliation pass, as the loop does on each
// tick.
func (p *Manager) ReconcileOnce(ctx context.Context) { p.reconcileOnce(ctx) }

// SetNow replaces the manager's clock. Call it before the pool serves anything.
func (p *Manager) SetNow(now func() time.Time) {
	p.clock.set(now)
}

// PodService is the sandbox service as the manager uses it.
type PodService = podService

// NewWithSandbox builds the manager on a sandbox service the test
// provides; Notifier is what that service tells about held pods.
func NewWithSandbox(cfg Config, log *slog.Logger, svc PodService) (*Manager, error) {
	p, err := newManager(cfg, log)
	if err != nil {
		return nil, err
	}
	p.sandbox = svc
	return p, nil
}

// Notifier is the hook the sandbox service tells about held pods.
func (p *Manager) Notifier() sandbox.Notifier { return notifier{p} }

// Reload replaces the served workspaces as a Sync does, from a list given
// directly: membership changes without the listing's debounce, settings
// without an overrides file, and lists Sync would never produce.
func (p *Manager) Reload(ctx context.Context, workspaces []WorkspaceConfig) (ReloadResult, error) {
	if _, err := p.up.Identity(ctx); err != nil {
		return ReloadResult{}, err
	}
	return p.reload(ctx, workspaces)
}

// Touch lets time pass for the lending policy and the leases, as every
// request does.
func (p *Manager) Touch() {
	p.mu.Lock()
	p.tickLocked()
	p.mu.Unlock()
	p.settle(context.Background())
}
