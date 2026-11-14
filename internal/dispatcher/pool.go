package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/sandbox"
)

// Pod states as the inventory lists them. The sandbox service says whether a
// pod is free, held, resetting or lost; a held pod is leased while it has a
// binding.
const (
	PodIdle      = "idle"
	PodLeased    = "leased"
	PodResetting = "resetting"
	PodLost      = "lost"
)

// PoolConfig configures a Pool.
type PoolConfig struct {
	MulticaURL string
	// Ticket is the PAT of the bot account the manager runs as. The manager
	// serves every workspace that account belongs to, found by Sync, and
	// registers its runtimes in local mode like a member's own daemon.
	Ticket string
	// Quota is the most slots each discovered workspace's running tasks may
	// occupy at once; 0 means no cap.
	Quota int
	// SyncInterval is how often Run looks for workspaces joined or left. Zero
	// means DefaultSyncInterval.
	SyncInterval time.Duration
	// RuntimeVisibility is set on every runtime the manager registers: "public"
	// (the default) lets every member of the workspace use it, "private" leaves
	// it to its owner until the owner shares it.
	RuntimeVisibility string
	// DaemonID is the one upstream daemon_id the manager uses in every
	// workspace. Empty derives it from MulticaURL and the Ticket's account,
	// which keeps it stable across restarts.
	DaemonID string
	// Name is the machine name and the default runtime name members see.
	Name string
	// UpstreamBackoff is how long a workspace is skipped after its upstream
	// failed a claim. Zero means DefaultUpstreamBackoff.
	UpstreamBackoff time.Duration
	// ExpectedSize is the number of pods the deployment declares, reported next
	// to the live counts so a shortfall is visible.
	ExpectedSize int
	// Providers limits which agent CLIs a pod reports become upstream
	// runtimes. Empty exposes every reported provider.
	Providers []string
	// HeartbeatTimeout, ResetTimeout and LostRetention configure the sandbox
	// service (see sandbox.Config); zero means its defaults.
	HeartbeatTimeout time.Duration
	ResetTimeout     time.Duration
	LostRetention    time.Duration
	// AuditLog receives the audit records as JSON Lines. Nil disables auditing.
	AuditLog io.Writer
	// Placement is the affinity rule that decides which tasks share a pod.
	// Empty means lending.RulePersonIssue.
	Placement lending.AffinityRule
	// Capacity is each pod's capacity in slots: how many tasks of one
	// binding run on it at once. Zero means DefaultCapacity.
	Capacity int
	// PlacementWait bounds how long a claimed task waits in the manager for a
	// pod that can take it. Zero means DefaultPlacementWait.
	PlacementWait time.Duration
	// WakeInterval is the dispatch tick, the only place tasks are claimed:
	// every interval the manager claims a task upstream for a pod it holds or
	// acquires, and tells the pod over its wake socket to pick it up instead
	// of waiting for its next poll. Zero serves no wake socket, so pods pick
	// their tasks up by polling, and ticks every DefaultDispatchInterval.
	WakeInterval time.Duration
	// HoldTimeout is how long a binding with no running task keeps its pod
	// for its affinity key before the pod is reset. Zero resets the pod as
	// soon as its last task ends; the command line defaults to
	// DefaultHoldTimeout.
	HoldTimeout time.Duration
	// PodMaxLifetime stops a shared pod from taking new tasks this long after
	// it was bound. Zero means DefaultPodMaxLifetime.
	PodMaxLifetime time.Duration
	// PodDrainGrace is how long the tasks still running at that point may
	// finish before the pod is released and they are failed. Zero means
	// DefaultPodDrainGrace.
	PodDrainGrace time.Duration
	// DrainTimeout is how long a workspace the account left may keep its
	// leases before they are ended. Zero means DefaultDrainTimeout.
	DrainTimeout time.Duration
	// DrainPoll is how often a removed workspace is checked for leftover
	// work. Zero means one second.
	DrainPoll time.Duration
	// ReconcileInterval turns on lease reconciliation (see reconcileOnce):
	// every interval each leased task is looked up in Multica, the leases of
	// tasks that are over are ended, and failure reports that did not get
	// through are sent again. Zero turns it off; the command line defaults to
	// DefaultReconcileInterval.
	ReconcileInterval time.Duration
	// LeaseMax is the longest a lease may last; a task still leased after it
	// is failed upstream and its lease ended. Zero means DefaultLeaseMax.
	LeaseMax time.Duration
	// ReadOverrides returns the per-workspace overrides file (see
	// ParseWorkspaceOverrides). It is read on every Sync, so an edited file
	// applies without a restart. Nil means no overrides.
	ReadOverrides func() ([]byte, error)
}

func (c *PoolConfig) normalize() error {
	if c.WakeInterval < 0 {
		return fmt.Errorf("wake interval must not be negative")
	}
	if c.UpstreamBackoff < 0 {
		return fmt.Errorf("upstream backoff must not be negative")
	}
	if c.UpstreamBackoff == 0 {
		c.UpstreamBackoff = DefaultUpstreamBackoff
	}
	if err := checkBaseURL(c.MulticaURL); err != nil {
		return fmt.Errorf("Multica URL: %w", err)
	}
	c.MulticaURL = strings.TrimRight(c.MulticaURL, "/")
	if c.ReconcileInterval < 0 {
		return fmt.Errorf("reconcile interval must not be negative")
	}
	if c.LeaseMax < 0 {
		return fmt.Errorf("maximum lease time must not be negative")
	}
	if c.LeaseMax == 0 {
		c.LeaseMax = DefaultLeaseMax
	}
	if c.DrainTimeout < 0 || c.DrainPoll < 0 {
		return fmt.Errorf("drain timeout and poll interval must not be negative")
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.DrainPoll == 0 {
		c.DrainPoll = time.Second
	}
	if c.ExpectedSize < 0 {
		return fmt.Errorf("expected pool size must not be negative")
	}
	if err := c.normalizePlacement(); err != nil {
		return err
	}
	if c.Name == "" {
		c.Name = "Managed Sandbox"
	}
	if c.Quota < 0 {
		return fmt.Errorf("quota must not be negative")
	}
	if c.SyncInterval < 0 {
		return fmt.Errorf("sync interval must not be negative")
	}
	if c.SyncInterval == 0 {
		c.SyncInterval = DefaultSyncInterval
	}
	switch c.RuntimeVisibility {
	case "":
		c.RuntimeVisibility = "public"
	case "public", "private":
	default:
		return fmt.Errorf("runtime visibility must be public or private")
	}
	c.DaemonID = strings.TrimSpace(c.DaemonID)
	c.Ticket = strings.TrimSpace(c.Ticket)
	if c.Ticket == "" {
		return fmt.Errorf("the bot account's ticket is required")
	}
	return nil // the workspaces arrive with the first Sync
}

// Pool is the pool manager: to Multica it is one daemon, to each pod it is the
// server. It reaches the pods themselves only through the sandbox service,
// which registers them, resets them and judges their liveness; the manager
// keeps no knowledge of how they are deployed.
//
// Pool is the Dispatcher: the only module that knows the others. It wires
// them together, owns the lending state (leases, claimed tasks, the held pods
// and the workspace records' live fields) under mu, and carries out the
// placement, lifetime and claim-selection decisions. The other modules keep
// their own state under their own locks and never call one another; the lock
// order is mu first, then any module's lock, and no module calls back into the
// Dispatcher while holding its own. The sandbox service is never called with
// mu held: its notifications take mu.
type Pool struct {
	cfg   PoolConfig
	log   *slog.Logger
	clock *clock
	// up is the Upstream module.
	up *upstreamClient
	// sandbox is the sandbox service the pods are reached through.
	sandbox podService
	// sandboxHandler serves the service's own pod paths on the pod listener;
	// sandboxRun runs its liveness checks. Both are nil for a service the
	// manager did not build.
	sandboxHandler http.Handler
	sandboxRun     func(context.Context)
	// ws is the Workspace sync & config module. The live state on its
	// workspace records is guarded by mu.
	ws *workspaceSet
	// api is the Pod API module. It asks the dispatcher for every decision and
	// never holds its own locks while doing so; the dispatcher may call it with
	// mu held.
	api *podAPI

	mu sync.Mutex
	// policy is the lending policy; every placement, claim-selection and
	// lifetime decision comes from it.
	policy *lending.Policy
	// held are the pods acquired from the sandbox service and not handed
	// back, by pod ID; heldSeq numbers their acquisitions.
	held    map[string]*pod
	heldSeq uint64
	// daemonWorkspaces are, by pod ID, the workspaces each pod's daemon
	// registered for, held or not.
	daemonWorkspaces map[string]map[string]struct{}
	// handBacks are the pods the policy handed back, not yet passed to the
	// sandbox service's Delete.
	handBacks []handBack
	// leases holds the active leases by task ID; history keeps them, closed
	// ones included, for the lease query.
	leases  map[string]*lease
	history []*lease
	// claimed holds, by task ID, the tasks claimed upstream that no pod has
	// picked up yet.
	claimed map[string]*parkedTask
	// forced holds tasks the policy decided to fail, not yet failed upstream.
	forced []forcedTask
	// overrides are the per-workspace overrides in effect.
	overrides overrideState
	// auditLog is nil when auditing is off.
	auditLog *auditLog
	// failRetry holds failure reports Multica did not take yet; the
	// reconcile loop sends them again.
	failRetry []pendingFail
	// orphansRecovered is set once the first successful Sync asked Multica to
	// fail what an earlier manager left running.
	orphansRecovered bool
}

// tickLocked lets time pass for the lending policy (holds, lifetimes,
// placement waits) and ends leases past the maximum lease time. Callers hold
// p.mu, and settle once they release it.
func (p *Pool) tickLocked() {
	p.applyLocked(p.policy.Tick())
	p.expireLeasesLocked()
}

// NewPool builds the pool manager with its own sandbox service.
func NewPool(cfg PoolConfig, log *slog.Logger) (*Pool, error) {
	p, err := newPool(cfg, log)
	if err != nil {
		return nil, err
	}
	svc, err := sandbox.NewGitOps(sandbox.Config{
		HeartbeatTimeout: p.cfg.HeartbeatTimeout, ResetTimeout: p.cfg.ResetTimeout, LostRetention: p.cfg.LostRetention,
		Notify: notifier{p}, Audit: sandboxAudit{p.auditLog}, Log: p.log, Now: p.now,
	})
	if err != nil {
		return nil, err
	}
	p.sandbox, p.sandboxHandler, p.sandboxRun = svc, svc.Handler(), svc.Run
	return p, nil
}

// newPool builds the pool manager without a sandbox service.
func newPool(cfg PoolConfig, log *slog.Logger) (*Pool, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pool{cfg: cfg, log: log, clock: newClock(), held: map[string]*pod{}, daemonWorkspaces: map[string]map[string]struct{}{},
		leases: map[string]*lease{}, claimed: map[string]*parkedTask{}}
	if err := p.newPolicy(); err != nil {
		return nil, err
	}
	p.up = newUpstreamClient(cfg.MulticaURL, cfg.Ticket, cfg.DaemonID, log)
	p.api = newPodAPI(log, p.clock, cfg.WakeInterval > 0, p, p.up.forward)
	p.auditLog = newAuditLog(cfg.AuditLog, p.clock, func(err error) { p.log.Error("write audit record", "error", err) })
	p.ws = newWorkspaceSet(log)
	return p, nil
}

// workspaceInfoLocked describes a workspace for the inventory. Callers hold
// p.mu.
func (p *Pool) workspaceInfoLocked(ws *workspace) WorkspaceInfo {
	hold, life := p.cfg.HoldTimeout, p.cfg.PodMaxLifetime
	if ws.cfg.HoldTimeout != nil {
		hold = *ws.cfg.HoldTimeout
	}
	if ws.cfg.MaxLifetime != nil {
		life = *ws.cfg.MaxLifetime
	}
	return WorkspaceInfo{
		WorkspaceID: ws.cfg.WorkspaceID, Slug: ws.cfg.Slug, Name: ws.cfg.RuntimeName,
		Leased: p.leasedLocked(ws), Quota: ws.cfg.Quota, Upstream: ws.healthInfo(),
		Placement: string(ws.cfg.Placement), HoldTimeout: hold.String(), MaxLifetime: life.String(),
	}
}

// now is the manager's current time.
func (p *Pool) now() time.Time { return p.clock.now() }

// Inventory reports pod counts by state and the pods themselves, in
// registration order: their state comes from the sandbox service, their
// bindings and leases from the dispatcher.
func (p *Pool) Inventory() Inventory {
	// Time passes first, so a hand-back it causes shows in the pod states.
	p.mu.Lock()
	p.tickLocked()
	p.mu.Unlock()
	p.settle(context.Background())
	pods := p.sandbox.Inventory()
	p.mu.Lock()
	inv := Inventory{ExpectedSize: p.cfg.ExpectedSize, PrunedTotal: pods.PrunedTotal, Pods: []PodInfo{}, Workspaces: []WorkspaceInfo{}}
	for _, ws := range p.ws.served() {
		inv.Workspaces = append(inv.Workspaces, p.workspaceInfoLocked(ws))
	}
	for _, ws := range p.ws.drainingList() {
		inv.Draining = append(inv.Draining, p.workspaceInfoLocked(ws))
	}
	inv.OverrideProblems = p.overrideProblemsLocked()
	inv.LastSync = p.ws.syncStatus()
	now := p.now()
	for _, l := range p.leases {
		if age := int64(now.Sub(l.StartedAt) / time.Second); age > inv.OldestLeaseAgeSeconds {
			inv.OldestLeaseAgeSeconds = age
		}
	}
	inv.PendingFailReports = len(p.failRetry)
	for _, sp := range pods.Pods {
		id := string(sp.ID)
		info := PodInfo{ID: id, Name: sp.Label, State: p.podStateLocked(sp), RegisteredAt: sp.RegisteredAt, LastSeen: sp.LastSeen}
		switch info.State {
		case PodIdle:
			inv.Idle++
		case PodLeased:
			inv.Leased++
		case PodResetting:
			inv.Resetting++
		case PodLost:
			inv.Lost++
		}
		if pd := p.held[id]; pd != nil {
			if l := firstLease(pd); l != nil {
				info.WorkspaceID = l.WorkspaceID
			}
			info.Initiators = initiatorsOf(pd)
			info.Binding = p.podBindingLocked(id, now)
		}
		inv.Pods = append(inv.Pods, info)
	}
	p.mu.Unlock()
	return inv
}

// podStateLocked is the inventory state of a pod the sandbox service lists:
// a free pod is idle, a held one leased while it has a binding or a task and
// idle otherwise. Callers hold p.mu.
func (p *Pool) podStateLocked(sp sandbox.PodInfo) string {
	switch sp.State {
	case sandbox.PodResetting:
		return PodResetting
	case sandbox.PodLost:
		return PodLost
	case sandbox.PodHeld:
		if pd := p.held[string(sp.ID)]; pd != nil {
			if _, bound := p.policy.Binding(pd.ID); bound || len(pd.leases) > 0 || len(pd.queue) > 0 {
				return PodLeased
			}
		}
	}
	return PodIdle
}

// AdminHandler serves the read-only admin API (see newAdminHandler).
func (p *Pool) AdminHandler() http.Handler {
	return newAdminHandler(p)
}

// PodHandler serves the standard daemon protocol to pods (see podAPI), and
// the sandbox service's own pod paths beside it.
func (p *Pool) PodHandler() http.Handler {
	return p.api.handler()
}

// unknownPod answers a call whose pod token names no pod (any more).
var unknownPod = &podError{status: http.StatusUnauthorized, msg: "unknown pod token"}

// registerPod registers the pod's daemon for a workspace: the workspace's
// runtimes are registered upstream on the first pod's behalf, and the pod is
// answered with them, filtered to the agent CLIs it offers.
func (p *Pool) registerPod(ctx context.Context, id string, req podRegistration) (map[string]json.RawMessage, *podError) {
	unavailable := &podError{status: http.StatusServiceUnavailable, msg: "runtime registration unavailable"}
	ws := p.workspaceByID(req.WorkspaceID)
	if order := p.workspaceList(); ws == nil && req.WorkspaceID == "" && len(order) == 1 {
		ws = order[0]
	}
	if ws == nil {
		return nil, refused("workspace is not served by this pool manager")
	}
	registration, err := p.ensureRegistered(ctx, ws, req.raw, req.headers)
	if err != nil {
		p.log.Warn("register pool runtime from pod", "pod_id", id, "workspace_id", ws.cfg.WorkspaceID, "error", err)
		return nil, unavailable
	}
	var resp map[string]json.RawMessage
	var runtimes []map[string]any
	if json.Unmarshal(registration, &resp) != nil || json.Unmarshal(resp["runtimes"], &runtimes) != nil {
		return nil, unavailable
	}
	kept := runtimes[:0]
	for _, rt := range runtimes {
		if provider, _ := rt["provider"].(string); req.offered[provider] {
			kept = append(kept, rt)
		}
	}
	resp["runtimes"], _ = json.Marshal(kept)

	wsID := ws.cfg.WorkspaceID
	p.mu.Lock()
	// The workspaces are kept whether or not the pod is held: the daemon
	// keeps running, and registered, through resets, so a pod acquired later
	// is lent to them.
	if p.daemonWorkspaces[id] == nil {
		p.daemonWorkspaces[id] = map[string]struct{}{}
	}
	p.daemonWorkspaces[id][wsID] = struct{}{}
	p.applyLocked(p.policy.PodRegistered(id, wsID))
	p.mu.Unlock()
	p.settle(ctx)
	p.log.Info("pod daemon registered", "pod_id", id, "workspace_id", wsID)
	return resp, nil
}

// ensureRegistered registers the pool's runtime upstream from the first pod's
// registration and returns the stored upstream response.
func (p *Pool) ensureRegistered(ctx context.Context, ws *workspace, captured json.RawMessage, headers map[string]string) (json.RawMessage, error) {
	ws.registerMu.Lock()
	defer ws.registerMu.Unlock()
	p.mu.Lock()
	registration := ws.registration
	p.mu.Unlock()
	body, err := p.upstreamRegistration(ws, captured)
	if len(registration) > 0 {
		if err != nil || coversProviders(registration, body) {
			return registration, nil
		}
		return p.registerMoreProviders(ctx, ws, body)
	}
	if err != nil {
		return nil, err
	}
	p.up.adoptHeaders(headers)
	raw, runtimes, err := p.up.register(ctx, ws.up, body)
	if err != nil {
		return nil, err
	}
	// What follows must not die with the pod's request that triggered it.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	p.mu.Lock()
	busy := p.workspaceBusyLocked(ws.cfg.WorkspaceID)
	p.mu.Unlock()
	if busy {
		// The workspace was registered before and still has work here (its
		// runtime was re-created upstream, or it came back after an absence):
		// the tasks on these runtimes are live, not orphans.
		p.log.Info("skipped orphan recovery: the workspace has active leases", "workspace_id", ws.cfg.WorkspaceID)
	} else {
		// Whatever Multica still shows running on these runtimes belongs to
		// no lease here and will never finish (the first Sync did the same for
		// the runtimes that existed then; these may be new). The workspace
		// holds no lease or claimed task in this manager, and none can be
		// claimed until the registration below is stored, so failing them
		// (which lets Multica retry) cannot hit a live task. Best effort: the
		// tasks would otherwise wait for Multica's own timeout.
		for _, rt := range runtimes {
			if err := p.up.recoverOrphans(bg, ws.up, rt.ID); err != nil {
				p.log.Warn("recover orphaned tasks upstream", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", rt.ID, "error", err)
			}
		}
	}
	p.mu.Lock()
	ws.registration = raw
	ws.shared = false
	for _, rt := range runtimes {
		delete(ws.goneRuntimes, rt.ID)
	}
	p.mu.Unlock()
	p.log.Info("registered pool runtimes", "workspace_id", ws.cfg.WorkspaceID, "runtimes", len(runtimes))
	p.shareRuntimes(bg, ws, runtimes)
	return raw, nil
}

// coversProviders reports whether the stored upstream registration has a
// runtime for every provider the registration body would register.
func coversProviders(stored json.RawMessage, body map[string]any) bool {
	var have struct {
		Runtimes []struct {
			Provider string `json:"provider"`
		} `json:"runtimes"`
	}
	if json.Unmarshal(stored, &have) != nil {
		return true
	}
	known := map[string]bool{}
	for _, rt := range have.Runtimes {
		known[rt.Provider] = true
	}
	want, _ := body["runtimes"].([]any)
	for _, item := range want {
		rt, _ := item.(map[string]any)
		if typ, _ := rt["type"].(string); !known[typ] {
			return false
		}
	}
	return true
}

// registerMoreProviders registers again, upstream, when a pod offers agent CLIs
// the stored registration has no runtime for (the image gained one while the
// manager kept running). Registering the same machine again only adds the new
// runtimes. The workspace's tasks and keys are untouched.
func (p *Pool) registerMoreProviders(ctx context.Context, ws *workspace, body map[string]any) (json.RawMessage, error) {
	raw, runtimes, err := p.up.register(ctx, ws.up, body)
	if err != nil {
		return nil, err
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	p.mu.Lock()
	ws.registration = raw
	ws.shared = false
	for _, rt := range runtimes {
		delete(ws.goneRuntimes, rt.ID)
	}
	p.mu.Unlock()
	p.log.Info("registered more pool runtimes", "workspace_id", ws.cfg.WorkspaceID, "runtimes", len(runtimes))
	p.shareRuntimes(bg, ws, runtimes)
	return raw, nil
}

// upstreamRegistration rewrites a pod daemon's registration into the
// manager's: one machine identity, local mode, no custom profiles.
func (p *Pool) upstreamRegistration(ws *workspace, captured json.RawMessage) (map[string]any, error) {
	id := registrationIdentity{
		workspaceID: ws.cfg.WorkspaceID, daemonID: p.up.knownIdentity(), name: ws.cfg.RuntimeName,
		providers: p.cfg.Providers, source: "the pod", mode: runtimeMode,
	}
	if old := perWorkspaceManagerID(p.cfg.MulticaURL, ws.cfg.WorkspaceID); old != id.daemonID {
		id.legacyIDs = []string{old}
	}
	return rewriteRegistration(captured, id)
}

// runtimeMode is the mode the pool's runtimes are registered in: the manager
// appears as the bot account's own local daemon.
const runtimeMode = "local"

// heartbeatPod keeps the pod's runtime online upstream. It says nothing of
// the pod's liveness, which the sandbox service judges from the control
// channel.
func (p *Pool) heartbeatPod(ctx context.Context, id, runtimeID string) *podError {
	p.mu.Lock()
	p.tickLocked()
	gone := p.runtimeGoneLocked(runtimeID)
	p.mu.Unlock()
	p.settle(ctx)
	if gone {
		// Multica deleted this runtime and the manager dropped the
		// registration; this answer makes the daemon register the workspace
		// again, which registers it upstream anew. The stock daemon does
		// that only on a 404 whose body says "runtime not found"
		// (daemon.isRuntimeNotFoundError).
		return &podError{status: http.StatusNotFound, msg: "runtime not found: the runtime was deleted upstream; register again"}
	}
	p.relayHeartbeat(runtimeID)
	return nil
}

// relayHeartbeat keeps the pool's upstream runtime online while a live pod
// heartbeats. The pods talk to the manager, not to Multica, so nothing else
// would. The upstream answer (pending pings, updates) is not passed on: the
// manager answers those itself. The call is made in the background so a slow
// upstream never delays a pod.
func (p *Pool) relayHeartbeat(runtimeID string) {
	ws := p.workspaceByRuntime(runtimeID)
	if ws == nil {
		return
	}
	if !p.up.heartbeatDue(runtimeID, p.now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), upstreamClaimTimeout)
		defer cancel()
		ack, err := p.up.heartbeat(ctx, ws.up, runtimeID)
		p.recordUpstream(ws, "heartbeat", err)
		if err == nil && ack.PendingModelList != nil && ack.PendingModelList.ID != "" {
			p.api.noteModelRequest(runtimeID, ack.PendingModelList.ID)
		}
		if err != nil {
			p.log.Warn("heartbeat runtime upstream", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", runtimeID, "error", err)
		}
		if runtimeNotFound(err) {
			p.dropRegistration(ws, runtimeID)
		}
	}()
}
