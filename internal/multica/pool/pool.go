// Package pool is the manager's pod side for Multica: it lends the core
// pods (it holds the sandbox service) and plays the Multica server toward the
// stock daemon in each pod (multica.DaemonAPI). It knows which tenants each
// pod's daemon registered for, holds the pod tokens and the wake sockets, and
// relays the pods' task reports upstream once the core said the task is the
// pod's. It keeps no record of which task runs where: the core does, and the
// pool asks it.
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/sandbox"
)

// Service is the sandbox service as the pool uses it: the Service interface,
// plus the in-process plumbing beside it.
type Service interface {
	sandbox.Service
	// Authenticate returns the pod that holds a pod token.
	Authenticate(token string) (sandbox.PodID, bool)
	// Inventory lists every pod with its state.
	Inventory() sandbox.Inventory
	// Pod reports one pod; false when the service does not know it.
	Pod(id sandbox.PodID) (sandbox.PodInfo, bool)
}

// Core is what the pool asks and tells the core.
type Core interface {
	Next(ctx context.Context, pod string) json.RawMessage
	Owns(pod, task string) bool
	TaskEnded(pod, task, reason string)
	MayServe(pod, tenant string) core.Verdict
	PodServes(pod, tenant string)
	PodLost(pod, why string)
	PodInterrupted(pod, why string)
	Touch()
}

// Source is what the pool asks the task source directly: the registrations,
// which do not touch the policy.
type Source interface {
	NotReady() string
	Tenants() []multica.WorkspaceRef
	Served(id string) (string, bool)
	Register(ctx context.Context, pod string, req multica.Registration) (map[string]json.RawMessage, string, *multica.PodError)
	RuntimeServed(runtimeID string) bool
	RuntimeGone(runtimeID string) bool
	ModelsReported(runtimeID string, report []byte)
}

// Config configures a Pool.
type Config struct {
	Log *slog.Logger
	Now func() time.Time
	// Service is the sandbox service the pods are reached through. Handler
	// serves its own pod paths on the pod listener, nil when it has none.
	Service Service
	Handler http.Handler
	// Client relays the pods' requests upstream.
	Client *multica.Client
	// Wake serves the wake socket; without it pods only poll.
	Wake bool
}

// Pool is the pod side.
type Pool struct {
	cfg    Config
	log    *slog.Logger
	svc    Service
	api    *multica.DaemonAPI
	core   Core
	source Source

	mu sync.Mutex
	// registered are, by pod ID, the tenants each pod's daemon registered
	// for, whether the core holds the pod or not.
	registered map[string]map[string]struct{}
}

// New returns the pool. Connect gives it the core and the source before it
// serves anything.
func New(cfg Config) *Pool {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p := &Pool{cfg: cfg, log: cfg.Log, svc: cfg.Service, registered: map[string]map[string]struct{}{}}
	podPaths := cfg.Handler
	if podPaths == nil {
		podPaths = http.NotFoundHandler()
	}
	p.api = multica.NewDaemonAPI(multica.DaemonAPIConfig{
		Log: cfg.Log, Now: cfg.Now, Wake: cfg.Wake, Backend: backend{p}, Forward: cfg.Client.Forward,
		PodPaths: map[string]http.Handler{sandbox.RegisterPath: podPaths, sandbox.ControlPath: podPaths},
	})
	return p
}

// Connect gives the pool the core and the source.
func (p *Pool) Connect(c Core, s Source) {
	p.core, p.source = c, s
}

// Handler serves the daemon protocol to the pods, and the sandbox service's
// own pod paths beside it.
func (p *Pool) Handler() http.Handler { return p.api.Handler() }

// Notifier is what the sandbox service tells about held pods: the pool
// passes it on to the core.
func (p *Pool) Notifier() sandbox.Notifier { return notifier{p} }

type notifier struct{ p *Pool }

func (n notifier) Interrupted(id sandbox.PodID, why string) { n.p.core.PodInterrupted(string(id), why) }
func (n notifier) Lost(id sandbox.PodID, why string)        { n.p.core.PodLost(string(id), why) }

// What the core asks (core.Pool).

// Acquire takes a free, reset pod from the sandbox service.
func (p *Pool) Acquire(ctx context.Context) (string, error) {
	id, err := p.svc.Create(ctx)
	if errors.Is(err, sandbox.ErrNoCapacity) {
		return "", core.ErrNoCapacity
	}
	return string(id), err
}

// HandBack hands a pod back to the sandbox service, which resets it.
func (p *Pool) HandBack(ctx context.Context, pod string, why core.HandBack) error {
	return p.svc.Delete(ctx, sandbox.PodID(pod), sandbox.AuditLabel{
		Reason: why.Reason, TaskID: why.TaskID, WorkspaceID: why.Tenant, Initiator: why.Initiator,
	})
}

func podInfo(sp sandbox.PodInfo) core.PodInfo {
	return core.PodInfo{ID: string(sp.ID), Name: sp.Label, State: core.PodState(sp.State), RegisteredAt: sp.RegisteredAt, LastSeen: sp.LastSeen}
}

// Pod reports one pod.
func (p *Pool) Pod(id string) (core.PodInfo, bool) {
	sp, ok := p.svc.Pod(sandbox.PodID(id))
	if !ok {
		return core.PodInfo{}, false
	}
	return podInfo(sp), true
}

// Pods lists every pod, in registration order, and forgets the registrations
// of pods the sandbox service no longer lists.
func (p *Pool) Pods() []core.PodInfo {
	inv := p.svc.Inventory()
	out := make([]core.PodInfo, 0, len(inv.Pods))
	known := map[string]bool{}
	for _, sp := range inv.Pods {
		out = append(out, podInfo(sp))
		known[string(sp.ID)] = true
	}
	p.mu.Lock()
	for id := range p.registered {
		if !known[id] {
			delete(p.registered, id)
		}
	}
	p.mu.Unlock()
	return out
}

// PrunedTotal counts the lost pods the sandbox service pruned.
func (p *Pool) PrunedTotal() int { return p.svc.Inventory().PrunedTotal }

// Tenants lists the tenants the pod's daemon registered for, sorted.
func (p *Pool) Tenants(pod string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.registered[pod]))
	for id := range p.registered[pod] {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Wake tells a pod over its wake socket that a task is placed on it.
func (p *Pool) Wake(pod string) { p.api.WakePod(pod) }

// Preferred reports whether the pod has a wake socket, so it picks a task up
// at once.
func (p *Pool) Preferred(pod string) bool { return p.api.Wakeable(pod) }

// What the source tells (tasksource.Pods).

// NotifyWorkspacesChanged makes the daemons read their workspace list again.
func (p *Pool) NotifyWorkspacesChanged(now bool) {
	if now {
		p.api.NotifyWorkspacesChangedNow()
	} else {
		p.api.NotifyWorkspacesChangedLimited()
	}
}

// NoteModelRequest hands a model list request to the pods of the runtime.
func (p *Pool) NoteModelRequest(runtimeID, id string) { p.api.NoteModelRequest(runtimeID, id) }

// HasLivePods reports whether any pod is not lost.
func (p *Pool) HasLivePods() bool {
	for _, sp := range p.svc.Inventory().Pods {
		if sp.State != sandbox.PodLost {
			return true
		}
	}
	return false
}

// The daemon protocol's questions (multica.Backend).

type backend struct{ p *Pool }

var _ multica.Backend = backend{}

func (b backend) NotReady() string { return b.p.source.NotReady() }

func (b backend) PodByToken(token string) (string, bool) {
	id, ok := b.p.svc.Authenticate(token)
	return string(id), ok
}

func (b backend) ServedWorkspaces() []multica.WorkspaceRef { return b.p.source.Tenants() }

func (b backend) ServedWorkspace(id string) (string, bool) { return b.p.source.Served(id) }

func (b backend) RuntimeServed(runtimeID string) bool { return b.p.source.RuntimeServed(runtimeID) }

// RegisterPod registers the pod's daemon for a workspace: the source
// registers the workspace's runtimes upstream on the first pod's behalf and
// answers, and the core learns the pod can serve the workspace. The
// registration is kept whether or not the core holds the pod: the daemon
// keeps running, and registered, through resets.
func (b backend) RegisterPod(ctx context.Context, podID string, reg multica.Registration) (map[string]json.RawMessage, *multica.PodError) {
	resp, tenant, perr := b.p.source.Register(ctx, podID, reg)
	if perr != nil {
		return nil, perr
	}
	b.p.mu.Lock()
	if b.p.registered[podID] == nil {
		b.p.registered[podID] = map[string]struct{}{}
	}
	b.p.registered[podID][tenant] = struct{}{}
	b.p.mu.Unlock()
	b.p.core.PodServes(podID, tenant)
	b.p.log.Info("pod daemon registered", "pod_id", podID, "workspace_id", tenant)
	return resp, nil
}

// HeartbeatPod takes a pod daemon's heartbeat. It says nothing of the pod's
// liveness, which the sandbox service judges from the control channel, and
// is not relayed: the source heartbeats the runtimes itself.
func (b backend) HeartbeatPod(ctx context.Context, podID, runtimeID string) *multica.PodError {
	b.p.core.Touch()
	if b.p.source.RuntimeGone(runtimeID) {
		// Multica deleted this runtime and the source dropped the
		// registration; this answer makes the daemon register the workspace
		// again, which registers it upstream anew. The stock daemon does
		// that only on a 404 whose body says "runtime not found"
		// (daemon.isRuntimeNotFoundError).
		return &multica.PodError{Status: http.StatusNotFound, Msg: "runtime not found: the runtime was deleted upstream; register again"}
	}
	return nil
}

func (b backend) ClaimFor(ctx context.Context, podID string) json.RawMessage {
	return b.p.core.Next(ctx, podID)
}

// ReposRoute is the route for the pod to read a workspace's repos and
// settings with the account's credential, when it may: it holds a task of the
// workspace, or it runs nothing and has no binding, registered for the
// workspace and is clean (free in the sandbox service, or held and idle). A
// pod working for another workspace, a pod between its lease's end and the
// end of its reset, and an unknown pod read nothing.
func (b backend) ReposRoute(podID, workspaceID string) (*multica.Route, bool) {
	switch b.p.core.MayServe(podID, workspaceID) {
	case core.Allowed:
		return &multica.Route{AccountAuth: true}, true
	case core.Denied:
		return nil, false
	case core.NotHeld:
		sp, ok := b.p.svc.Pod(sandbox.PodID(podID))
		if !ok || sp.State != sandbox.PodFree {
			return nil, false
		}
	}
	b.p.mu.Lock()
	_, ok := b.p.registered[podID][workspaceID]
	b.p.mu.Unlock()
	if !ok {
		return nil, false
	}
	return &multica.Route{AccountAuth: true}, true
}

// ModelsReported hands a pod's model list report to the source, which reuses
// it for every runtime of the provider.
func (b backend) ModelsReported(runtimeID string, report []byte) {
	b.p.source.ModelsReported(runtimeID, report)
}

// TaskRoute is the route for a pod's report on a task, provided the task is
// leased to that pod. A terminal report (complete, fail, cancel-ack) ends the
// lease once Multica took it. It also ends when Multica refuses it with 401,
// 403 or 404: the PAT lost access, or the task or its runtime is gone, so
// Multica no longer knows the task and nothing will ever end it there.
func (b backend) TaskRoute(podID, taskID, action, path string) (*multica.Route, bool) {
	if !b.p.core.Owns(podID, taskID) {
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
			b.p.log.Warn("lease ended: upstream refused a terminal report", "task_id", taskID, "action", action, "status", status)
		default:
			return
		}
		b.p.core.TaskEnded(podID, taskID, reason)
	}}
	if multica.TaskTokenPath(path) {
		// The server authorizes these with the task-scoped token the daemon
		// carries; it must reach the server unchanged.
		route.AccountAuth = false
	}
	return route, true
}

// Registered reports whether the pod's daemon registered for the tenant.
func (p *Pool) Registered(pod, tenant string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.registered[pod][tenant]
	return ok
}
