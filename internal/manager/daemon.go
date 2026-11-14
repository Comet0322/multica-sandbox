package manager

import (
	"context"
	"encoding/json"

	"sandbox-manager/internal/multica"
)

// daemonBackend answers the DaemonAPI's questions about pods for the
// manager, which keeps the methods to itself.
type daemonBackend struct{ p *Manager }

var _ multica.Backend = daemonBackend{}

func (b daemonBackend) NotReady() string                       { return b.p.notReady() }
func (b daemonBackend) PodByToken(token string) (string, bool) { return b.p.podByToken(token) }
func (b daemonBackend) ServedWorkspaces() []multica.WorkspaceRef {
	return b.p.servedWorkspaces()
}

func (b daemonBackend) RegisterPod(ctx context.Context, podID string, reg multica.Registration) (map[string]json.RawMessage, *multica.PodError) {
	return b.p.registerPod(ctx, podID, reg)
}

func (b daemonBackend) HeartbeatPod(ctx context.Context, podID, runtimeID string) *multica.PodError {
	return b.p.heartbeatPod(ctx, podID, runtimeID)
}

func (b daemonBackend) ClaimFor(ctx context.Context, podID string) json.RawMessage {
	return b.p.claimFor(ctx, podID)
}

func (b daemonBackend) ServedWorkspace(id string) (string, bool) { return b.p.servedWorkspace(id) }

func (b daemonBackend) ReposRoute(podID, workspaceID string) (*multica.Route, bool) {
	return b.p.reposRoute(podID, workspaceID)
}

func (b daemonBackend) RuntimeServed(runtimeID string) bool { return b.p.runtimeServed(runtimeID) }

func (b daemonBackend) TaskRoute(podID, taskID, action, path string) (*multica.Route, bool) {
	return b.p.taskRoute(podID, taskID, action, path)
}
