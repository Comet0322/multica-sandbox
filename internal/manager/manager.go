package manager

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
	"sandbox-manager/internal/multica"
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

// Config configures a Manager.
type Config struct {
	MulticaURL string
	// PAT is the PAT of the bot account the manager runs as. The manager
	// serves every workspace that account belongs to, found by Sync, and
	// registers its runtimes in local mode like a member's own daemon.
	PAT string
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
	// workspace. Empty derives it from MulticaURL and the PAT's account,
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
	// Empty means multica.DefaultAffinityRule.
	AffinityRule lending.AffinityRule
	// Capacity is each pod's capacity in slots: how many tasks of one
	// binding run on it at once. Zero means DefaultCapacity.
	Capacity int
	// PlacementWait bounds how long a claimed task waits in the manager for a
	// pod that can take it. Zero means DefaultPlacementWait.
	PlacementWait time.Duration
	// DispatchInterval is the dispatch tick, the only place tasks are claimed:
	// every interval the manager claims a task upstream for a pod it holds or
	// acquires, and tells the pod over its wake socket to pick it up instead
	// of waiting for its next poll. Zero serves no wake socket, so pods pick
	// their tasks up by polling, and ticks every DefaultDispatchInterval.
	DispatchInterval time.Duration
	// HoldTimeout is how long a binding with no running task keeps its pod
	// for its affinity key before the pod is reset. Zero resets the pod as
	// soon as its last task ends; the command line defaults to
	// DefaultHoldTimeout.
	HoldTimeout time.Duration
	// MaxLifetime stops a shared pod from taking new tasks this long after
	// it was bound. Zero means DefaultMaxLifetime.
	MaxLifetime time.Duration
	// DrainGrace is how long the tasks still running at that point may
	// finish before the pod is released and they are failed. Zero means
	// DefaultDrainGrace.
	DrainGrace time.Duration
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

func (c *Config) normalize() error {
	if c.DispatchInterval < 0 {
		return fmt.Errorf("wake interval must not be negative")
	}
	if c.UpstreamBackoff < 0 {
		return fmt.Errorf("upstream backoff must not be negative")
	}
	if c.UpstreamBackoff == 0 {
		c.UpstreamBackoff = DefaultUpstreamBackoff
	}
	if err := multica.CheckBaseURL(c.MulticaURL); err != nil {
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
	if err := c.normalizeLending(); err != nil {
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
	c.PAT = strings.TrimSpace(c.PAT)
	if c.PAT == "" {
		return fmt.Errorf("the bot account's PAT is required")
	}
	return nil // the workspaces arrive with the first Sync
}

// Manager is the sandbox manager: to Multica it is one daemon, to each pod it
// is the server. It reaches the pods themselves only through the sandbox
// service, which registers them, resets them and judges their liveness; the
// manager keeps no knowledge of how they are deployed.
//
// Manager is the only module that knows the others: the Multica client and
// each workspace's runtimes (package multica), the daemon protocol it serves
// to the pods (multica.DaemonAPI), the lending policy and the sandbox
// service. It wires them together, owns the lending state (leases, claimed
// tasks, the held pods and the workspace records' live fields) under mu, and
// carries out the placement, lifetime and claim-selection decisions. The
// other modules keep their own state under their own locks and never call one
// another; the lock order is mu first, then any module's lock, and no module
// calls back into the manager while holding its own. The sandbox service is
// never called with mu held: its notifications take mu.
type Manager struct {
	cfg   Config
	log   *slog.Logger
	clock *clock
	// up is the bot account on the Multica server.
	up *multica.Client
	// sandbox is the sandbox service the pods are reached through.
	sandbox podService
	// sandboxHandler serves the service's own pod paths on the pod listener;
	// sandboxRun runs its liveness checks. Both are nil for a service the
	// manager did not build.
	sandboxHandler http.Handler
	sandboxRun     func(context.Context)
	// ws is the set of served workspaces. The live state on its workspace
	// records is guarded by mu.
	ws *workspaceSet
	// api serves the daemon protocol to the pods. It asks the manager for
	// every decision and never holds its own locks while doing so; the
	// manager may call it with mu held.
	api *multica.DaemonAPI

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
	claimed map[string]*claimedTask
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
func (p *Manager) tickLocked() {
	p.applyLocked(p.policy.Tick())
	p.expireLeasesLocked()
}

// New builds the manager with its own sandbox service.
func New(cfg Config, log *slog.Logger) (*Manager, error) {
	p, err := newManager(cfg, log)
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

// newManager builds the manager without a sandbox service.
func newManager(cfg Config, log *slog.Logger) (*Manager, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Manager{cfg: cfg, log: log, clock: newClock(), held: map[string]*pod{}, daemonWorkspaces: map[string]map[string]struct{}{},
		leases: map[string]*lease{}, claimed: map[string]*claimedTask{}}
	if err := p.newPolicy(); err != nil {
		return nil, err
	}
	p.up = multica.NewClient(cfg.MulticaURL, cfg.PAT, cfg.DaemonID, log)
	sandboxPaths := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.sandboxPaths().ServeHTTP(w, r) })
	p.api = multica.NewDaemonAPI(multica.DaemonAPIConfig{
		Log: log, Now: p.now, Wake: cfg.DispatchInterval > 0, Backend: daemonBackend{p}, Forward: p.up.Forward,
		PodPaths: map[string]http.Handler{sandbox.RegisterPath: sandboxPaths, sandbox.ControlPath: sandboxPaths},
	})
	p.auditLog = newAuditLog(cfg.AuditLog, p.clock, func(err error) { p.log.Error("write audit record", "error", err) })
	p.ws = newWorkspaceSet(log)
	return p, nil
}

// workspaceInfoLocked describes a workspace for the inventory. Callers hold
// p.mu.
func (p *Manager) workspaceInfoLocked(ws *workspace) WorkspaceInfo {
	hold, life := p.cfg.HoldTimeout, p.cfg.MaxLifetime
	if ws.cfg.HoldTimeout != nil {
		hold = *ws.cfg.HoldTimeout
	}
	if ws.cfg.MaxLifetime != nil {
		life = *ws.cfg.MaxLifetime
	}
	return WorkspaceInfo{
		WorkspaceID: ws.cfg.WorkspaceID, Slug: ws.cfg.Slug, Name: ws.cfg.RuntimeName,
		Leased: p.leasedLocked(ws), Quota: ws.cfg.Quota, Upstream: ws.rt.Health(),
		AffinityRule: string(ws.cfg.AffinityRule), HoldTimeout: hold.String(), MaxLifetime: life.String(),
	}
}

// now is the manager's current time.
func (p *Manager) now() time.Time { return p.clock.now() }

// Inventory reports pod counts by state and the pods themselves, in
// registration order: their state comes from the sandbox service, their
// bindings and leases from the manager.
func (p *Manager) Inventory() Inventory {
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
func (p *Manager) podStateLocked(sp sandbox.PodInfo) string {
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
func (p *Manager) AdminHandler() http.Handler {
	return newAdminHandler(p)
}

// PodHandler serves the standard daemon protocol to pods (see multica.DaemonAPI), and
// the sandbox service's own pod paths beside it.
func (p *Manager) PodHandler() http.Handler {
	return p.api.Handler()
}

// registerPod registers the pod's daemon for a workspace: the workspace's
// runtimes are registered upstream on the first pod's behalf, and the pod is
// answered with them, filtered to the agent CLIs it offers.
func (p *Manager) registerPod(ctx context.Context, id string, req multica.Registration) (map[string]json.RawMessage, *multica.PodError) {
	unavailable := &multica.PodError{Status: http.StatusServiceUnavailable, Msg: "runtime registration unavailable"}
	ws := p.workspaceByID(req.WorkspaceID)
	if order := p.workspaceList(); ws == nil && req.WorkspaceID == "" && len(order) == 1 {
		ws = order[0]
	}
	if ws == nil {
		return nil, multica.Refuse("workspace is not served by this sandbox manager")
	}
	registration, err := ws.rt.Register(ctx, req.Raw, req.Headers, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.workspaceBusyLocked(ws.cfg.WorkspaceID)
	})
	if err != nil {
		p.log.Warn("register the workspace's runtimes from a pod", "pod_id", id, "workspace_id", ws.cfg.WorkspaceID, "error", err)
		return nil, unavailable
	}
	resp, ok := multica.AnswerForPod(registration, req.Offered)
	if !ok {
		return nil, unavailable
	}

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

// heartbeatPod keeps the pod's runtime online upstream. It says nothing of
// the pod's liveness, which the sandbox service judges from the control
// channel.
func (p *Manager) heartbeatPod(ctx context.Context, id, runtimeID string) *multica.PodError {
	p.mu.Lock()
	p.tickLocked()
	p.mu.Unlock()
	p.settle(ctx)
	if p.runtimeGone(runtimeID) {
		// Multica deleted this runtime and the manager dropped the
		// registration; this answer makes the daemon register the workspace
		// again, which registers it upstream anew. The stock daemon does
		// that only on a 404 whose body says "runtime not found"
		// (daemon.isRuntimeNotFoundError).
		return &multica.PodError{Status: http.StatusNotFound, Msg: "runtime not found: the runtime was deleted upstream; register again"}
	}
	p.relayHeartbeat(runtimeID)
	return nil
}

// relayHeartbeat keeps the workspace's upstream runtime online while a live pod
// heartbeats. The pods talk to the manager, not to Multica, so nothing else
// would. The upstream answer (pending pings, updates) is not passed on: the
// manager answers those itself. The call is made in the background so a slow
// upstream never delays a pod.
func (p *Manager) relayHeartbeat(runtimeID string) {
	ws := p.workspaceByRuntime(runtimeID)
	if ws == nil {
		return
	}
	if !p.up.HeartbeatDue(runtimeID, p.now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), multica.ClaimTimeout)
		defer cancel()
		modelRequest, err := ws.rt.Heartbeat(ctx, runtimeID)
		if modelRequest != "" {
			p.api.NoteModelRequest(runtimeID, modelRequest)
		}
		if err != nil {
			p.log.Warn("heartbeat runtime upstream", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", runtimeID, "error", err)
		}
		if multica.IsRuntimeNotFound(err) {
			p.dropRegistration(ws, runtimeID)
		}
	}()
}
