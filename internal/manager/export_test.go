package manager

import (
	"context"
	"log/slog"
	"time"

	"sandbox-manager/internal/multica/pool"
	"sandbox-manager/internal/sandbox"
)

// DispatchOnce runs one active-dispatch round, as Run does on each tick.
func (m *Manager) DispatchOnce(ctx context.Context) { m.core.DispatchOnce(ctx) }

// ReconcileOnce runs one lease reconciliation pass and resends the failure
// reports that did not get through, as the loops do on each tick.
func (m *Manager) ReconcileOnce(ctx context.Context) {
	m.core.ReconcileOnce(ctx)
	m.source.RetryFailures(ctx)
}

// HeartbeatOnce heartbeats the runtimes upstream once, as the loop does.
func (m *Manager) HeartbeatOnce(ctx context.Context) { m.source.HeartbeatOnce(ctx) }

// SetNow replaces the manager's clock. Call it before the pool serves anything.
func (m *Manager) SetNow(now func() time.Time) { m.clock.Set(now) }

// PodService is the sandbox service as the manager uses it.
type PodService = pool.Service

// NewWithSandbox builds the manager on a sandbox service the test
// provides; Notifier is what that service tells about held pods.
func NewWithSandbox(cfg Config, log *slog.Logger, svc PodService) (*Manager, error) {
	return build(cfg, log, svc, nil)
}

// Notifier is the hook the sandbox service tells about held pods.
func (m *Manager) Notifier() sandbox.Notifier { return m.real.Notifier() }

// Reload replaces the served workspaces as a Sync does, from a list given
// directly: membership changes without the listing's debounce, settings
// without an overrides file, and lists Sync would never produce.
func (m *Manager) Reload(ctx context.Context, workspaces []WorkspaceConfig) (ReloadResult, error) {
	return m.source.Reload(ctx, workspaces)
}

// Touch lets time pass for the lending policy and the leases, as every
// request does.
func (m *Manager) Touch() { m.core.Touch() }
