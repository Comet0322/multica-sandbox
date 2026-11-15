package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/multica/pool"
	"sandbox-manager/internal/multica/tasksource"
	"sandbox-manager/internal/policy"
	"sandbox-manager/internal/sandbox"
)

// Defaults the manager applies to a zero Config field, or the command line
// does when nothing is set.
const (
	// DefaultSyncInterval is how often the manager looks for workspaces its
	// account joined or left.
	DefaultSyncInterval = tasksource.DefaultSyncInterval
	// DefaultUpstreamBackoff is how long a workspace whose upstream failed
	// is left alone before the manager claims from it again.
	DefaultUpstreamBackoff = 10 * time.Second
	// DefaultDispatchInterval is the dispatch tick when DispatchInterval is
	// zero.
	DefaultDispatchInterval = core.DefaultDispatchInterval
	// DefaultReconcileInterval is the reconcile period the command line uses
	// when none is set.
	DefaultReconcileInterval = 30 * time.Second
	// DefaultLeaseMax bounds a lease when no maximum lease time is set.
	DefaultLeaseMax = core.DefaultLeaseMax
	// DefaultDrainTimeout is how long a workspace removed may keep its leased
	// tasks before they are ended.
	DefaultDrainTimeout = core.DefaultDrainTimeout
	// DefaultHoldTimeout is the hold timeout the command line applies when
	// none is configured.
	DefaultHoldTimeout = 10 * time.Minute
	// DefaultPodMaxIdle is how long an idle pool pod is kept when none is
	// configured.
	DefaultPodMaxIdle = 24 * time.Hour
)

// Names the manager's packages share, kept here so callers need only this
// package.
type (
	WorkspaceConfig   = tasksource.WorkspaceConfig
	WorkspaceOverride = tasksource.WorkspaceOverride
	ReloadResult      = tasksource.ReloadResult
	SyncStatus        = tasksource.SyncStatus
	UpstreamHealth    = multica.Health
	LeaseInfo         = core.LeaseInfo
	AuditRecord       = core.AuditRecord
)

// Audit actions and decisions (see core.AuditRecord).
const (
	AuditLease   = core.AuditLease
	AuditRelease = core.AuditRelease
	AuditReset   = core.AuditReset
	AuditAllow   = core.AuditAllow
	AuditDeny    = core.AuditDeny
)

// ParseWorkspaceOverrides reads an overrides file (see
// tasksource.ParseWorkspaceOverrides).
func ParseWorkspaceOverrides(raw []byte) (map[string]WorkspaceOverride, map[string]error, error) {
	return tasksource.ParseWorkspaceOverrides(raw)
}

// Pod states as the inventory lists them. The pool says whether a pod is
// free, held, resetting or lost; a held pod is leased while it has a binding
// or a task.
const (
	PodIdle      = "idle"
	PodLeased    = "leased"
	PodResetting = "resetting"
	PodLost      = "lost"
)

// Config configures a Manager.
type Config struct {
	MulticaURL string
	// Kube, when set, has the sandbox service create pods through the
	// Kubernetes API from a pod template (ADR 0009) instead of serving
	// GitOps pods. Its Notify, Audit, Log and Now are the manager's.
	Kube *sandbox.KubeConfig
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
	// HeartbeatInterval is how often the runtimes are heartbeated upstream.
	// Zero means tasksource.DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// RuntimeVisibility is set on every runtime the manager registers: "public"
	// (the default) lets every member of the workspace use it, "private" leaves
	// it to its owner until the owner shares it.
	RuntimeVisibility string
	// DaemonID is the one upstream daemon_id the manager uses in every
	// workspace. Empty derives it from MulticaURL and the PAT's account,
	// which keeps it stable across restarts.
	DaemonID string
	// Name is the machine name Multica's runtime list shows as the title;
	// each runtime is named "<agent CLI> (Name)", as the stock daemon names
	// its runtimes after its host.
	Name string
	// DeviceName is shown under the title (the registration's device_name).
	// Empty means Name.
	DeviceName string
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
	// Models are model list reports, by provider, that answer Multica's model
	// picker while no pod has reported lately. Nil asks a pod.
	Models map[string]json.RawMessage
	// AffinityRule, Capacity, PlacementWait, MaxLifetime and DrainGrace
	// are checked but no longer used: a pod runs one task at a time and is
	// held for its task's key (ADR 0009). Issue 07 removes them.
	AffinityRule  multica.AffinityRule
	Capacity      int
	PlacementWait time.Duration
	MaxLifetime   time.Duration
	DrainGrace    time.Duration
	// Runtimes are the runtime kinds the manager registers in every
	// workspace and claims for (SANDBOX_RUNTIMES). Empty means the pool
	// kind. Each kind's runtimes are a machine of their own upstream: the
	// pool kind's daemon_id is DaemonID, another kind's DaemonID followed
	// by "-<kind>".
	Runtimes []policy.Kind
	// CleanName and CleanDeviceName are Name and DeviceName for the clean
	// runtimes. Empty means Name followed by " Clean", and CleanName.
	CleanName       string
	CleanDeviceName string
	// PodMaxIdle is how long an idle pool pod is kept before it is deleted.
	// Zero means DefaultPodMaxIdle.
	PodMaxIdle time.Duration
	// DispatchInterval is the dispatch tick, the only place tasks are claimed:
	// every interval the manager claims a task upstream for a pod it holds or
	// acquires, and tells the pod over its wake socket to pick it up instead
	// of waiting for its next poll. Zero serves no wake socket, so pods pick
	// their tasks up by polling, and ticks every DefaultDispatchInterval.
	DispatchInterval time.Duration
	// HoldTimeout is how long a pod is held for its task's key once the
	// task ended. Zero holds nothing; the command line defaults to
	// DefaultHoldTimeout.
	HoldTimeout time.Duration
	// DrainTimeout is how long a workspace the account left may keep its
	// leases before they are ended. Zero means DefaultDrainTimeout.
	DrainTimeout time.Duration
	// DrainPoll is how often a removed workspace is checked for leftover
	// work. Zero means one second.
	DrainPoll time.Duration
	// ReconcileInterval turns on lease reconciliation: every interval each
	// leased task is looked up in Multica, the leases of tasks that are over
	// are ended, and failure reports that did not get through are sent
	// again. Zero turns it off; the command line defaults to
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
	if c.DrainTimeout < 0 || c.DrainPoll < 0 {
		return fmt.Errorf("drain timeout and poll interval must not be negative")
	}
	if c.ExpectedSize < 0 {
		return fmt.Errorf("expected pool size must not be negative")
	}
	if err := multica.CheckAffinityRule(c.AffinityRule); err != nil {
		return err
	}
	if c.AffinityRule == "" {
		c.AffinityRule = multica.DefaultAffinityRule
	}
	if c.Capacity < 0 || c.PlacementWait < 0 || c.MaxLifetime < 0 || c.DrainGrace < 0 {
		return fmt.Errorf("pod capacity, placement wait, pod maximum lifetime and drain grace must not be negative")
	}
	if c.HoldTimeout < 0 || c.PodMaxIdle < 0 {
		return fmt.Errorf("hold timeout and pod maximum idle time must not be negative")
	}
	if c.PodMaxIdle == 0 {
		c.PodMaxIdle = DefaultPodMaxIdle
	}
	if c.Name == "" {
		c.Name = "Managed Sandbox"
	}
	if c.DeviceName == "" {
		c.DeviceName = c.Name
	}
	if c.CleanName == "" {
		c.CleanName = c.Name + " Clean"
	}
	if c.CleanDeviceName == "" {
		c.CleanDeviceName = c.CleanName
	}
	for _, name := range []string{c.Name, c.DeviceName, c.CleanName, c.CleanDeviceName} {
		if err := multica.CheckMachineName(name); err != nil {
			return fmt.Errorf("machine and device name: %w", err)
		}
	}
	if len(c.Runtimes) == 0 {
		c.Runtimes = []policy.Kind{policy.KindPool}
	}
	seen := map[policy.Kind]bool{}
	for _, k := range c.Runtimes {
		if k != policy.KindPool && k != policy.KindClean {
			return fmt.Errorf("unknown runtime kind %q (want pool or clean)", k)
		}
		if seen[k] {
			return fmt.Errorf("runtime kind %s is listed twice", k)
		}
		seen[k] = true
	}
	if c.Quota < 0 {
		return fmt.Errorf("quota must not be negative")
	}
	if c.SyncInterval < 0 || c.HeartbeatInterval < 0 {
		return fmt.Errorf("sync and heartbeat intervals must not be negative")
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
// is the server. It is assembled from three parts that know only the
// interfaces between them:
//
//   - the core (package core) decides which pod runs which task and keeps the
//     record of every task it dispatched;
//   - the task source (package tasksource) is the daemon Multica sees;
//   - the pool (package pool) holds the sandbox service and plays the server
//     toward the stock daemon in each pod.
//
// The manager wires them together and serves the admin API from what each
// reports.
type Manager struct {
	cfg    Config
	log    *slog.Logger
	clock  *core.Clock
	core   *core.Core
	source *tasksource.Source
	pool   PodSide
	// real is the pool side when it is the real one (package pool), nil for
	// a simulated one.
	real *pool.Pool
	// podRun runs the pod side's own loops (the sandbox service's liveness
	// checks, a simulation); nil when it has none.
	podRun func(context.Context)
}

// PodSide is the pod side as the manager wires it: what the core asks of a
// pool, what the task source tells the pods, the handler served to the pods,
// and how many lost pods were pruned, for the inventory.
type PodSide interface {
	core.Pool
	tasksource.Pods
	Handler() http.Handler
	PrunedTotal() int
}

// SimulatedPods builds a pod side that stands in for the real pool, given
// the core it serves, the task source it registers workspaces with and the
// bot account's client, and returns it with the loop that runs it.
type SimulatedPods func(c *core.Core, s *tasksource.Source, client *multica.Client) (PodSide, func(context.Context))

// forwardNotifier passes the sandbox service's notifications to the pool,
// which is built after the service.
type forwardNotifier struct{ m *Manager }

func (n forwardNotifier) Interrupted(id sandbox.PodID, why string) {
	n.m.real.Notifier().Interrupted(id, why)
}
func (n forwardNotifier) Lost(id sandbox.PodID, why string) { n.m.real.Notifier().Lost(id, why) }

// sandboxAudit writes the sandbox service's audit records to the manager's
// audit log, which shares their shape.
type sandboxAudit struct{ log *core.AuditLog }

func (a sandboxAudit) Record(rec sandbox.AuditRecord) { a.log.Write(core.AuditRecord(rec)) }

// New builds the manager with its own sandbox service.
func New(cfg Config, log *slog.Logger) (*Manager, error) {
	return build(cfg, log, nil, nil)
}

// NewSimulated builds the manager on a simulated pod side instead of the
// sandbox service and the pods' daemons, for development against a real
// Multica server.
func NewSimulated(cfg Config, log *slog.Logger, sim SimulatedPods) (*Manager, error) {
	return build(cfg, log, nil, sim)
}

// build assembles the manager on the simulated pod side sim, on svc, or on a
// sandbox service of its own when both are nil.
func build(cfg Config, log *slog.Logger, svc pool.Service, sim SimulatedPods) (*Manager, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{cfg: cfg, log: log, clock: core.NewClock()}
	audit := core.NewAuditLog(cfg.AuditLog, m.clock, func(err error) { log.Error("write audit record", "error", err) })
	var handler http.Handler
	// maxPods is the budget the policy keeps; zero leaves it to the pool,
	// as for the GitOps pods, whose number is fixed.
	maxPods := 0
	if svc == nil && sim == nil {
		gitops, err := sandbox.NewGitOps(sandbox.Config{
			HeartbeatTimeout: cfg.HeartbeatTimeout, ResetTimeout: cfg.ResetTimeout, LostRetention: cfg.LostRetention,
			Notify: forwardNotifier{m}, Audit: sandboxAudit{audit}, Log: log, Now: m.clock.Now,
		})
		if err != nil {
			return nil, err
		}
		svc, handler, m.podRun = gitops, gitops.Handler(), gitops.Run
		if cfg.Kube != nil {
			kc := *cfg.Kube
			kc.Notify, kc.Audit, kc.Log, kc.Now = forwardNotifier{m}, sandboxAudit{audit}, log, m.clock.Now
			kube, err := sandbox.NewKube(kc)
			if err != nil {
				return nil, err
			}
			svc, handler, m.podRun = kube, nil, kube.Run
			maxPods = kube.MaxPods()
		}
	}
	c, err := core.New(core.Config{
		Log: log, Clock: m.clock, Audit: audit, RemovedReason: "workspace removed",
		Kinds: cfg.Runtimes, HoldTimeout: cfg.HoldTimeout, MaxIdle: cfg.PodMaxIdle, MaxPods: maxPods, Backoff: cfg.UpstreamBackoff,
		DispatchInterval: cfg.DispatchInterval, ReconcileInterval: cfg.ReconcileInterval, LeaseMax: cfg.LeaseMax,
		DrainTimeout: cfg.DrainTimeout, DrainPoll: cfg.DrainPoll,
	})
	if err != nil {
		return nil, err
	}
	client := multica.NewClient(cfg.MulticaURL, cfg.PAT, cfg.DaemonID, log)
	var sets []tasksource.RuntimeSet
	for _, k := range cfg.Runtimes {
		set := tasksource.RuntimeSet{Kind: k, Name: cfg.Name, DeviceName: cfg.DeviceName}
		if k == policy.KindClean {
			set.Name, set.DeviceName = cfg.CleanName, cfg.CleanDeviceName
		}
		sets = append(sets, set)
	}
	src := tasksource.New(tasksource.Config{
		Log: log, Now: m.clock.Now, Client: client, Name: cfg.Name, DeviceName: cfg.DeviceName, Providers: cfg.Providers, Runtimes: sets,
		RuntimeVisibility: cfg.RuntimeVisibility, Quota: cfg.Quota, AffinityRule: cfg.AffinityRule,
		SyncInterval: cfg.SyncInterval, HeartbeatInterval: cfg.HeartbeatInterval, ReadOverrides: cfg.ReadOverrides,
		Models: cfg.Models,
	})
	m.core, m.source = c, src
	if sim != nil {
		m.pool, m.podRun = sim(c, src, client)
	} else {
		pl := pool.New(pool.Config{Log: log, Now: m.clock.Now, Service: svc, Handler: handler, Client: client, Wake: cfg.DispatchInterval > 0})
		pl.Connect(c, src)
		m.pool, m.real = pl, pl
	}
	c.Connect(src, m.pool)
	src.Connect(c, m.pool)
	return m, nil
}

// Run runs the sandbox service's liveness checks, the dispatch tick, the
// upstream heartbeats and, with ReconcileInterval set, the lease
// reconciliation and the retry of failure reports, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if m.podRun != nil {
		go m.podRun(ctx)
	}
	go m.source.HeartbeatLoop(ctx)
	if m.cfg.ReconcileInterval > 0 {
		go func() {
			t := time.NewTicker(m.cfg.ReconcileInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					m.source.RetryFailures(ctx)
				}
			}
		}()
	}
	m.core.Run(ctx)
}

// Restore takes over the pods a previous run left (see pool.Pool.Restore):
// running tasks run on, holds resume and warm and idle pods are kept. Call it
// once, before Sync and Run.
func (m *Manager) Restore(ctx context.Context) error {
	if m.real == nil {
		return nil
	}
	return m.real.Restore(ctx)
}

// Sync brings the served workspaces in line with the account's memberships
// (see tasksource.Source.Sync).
func (m *Manager) Sync(ctx context.Context) (ReloadResult, error) { return m.source.Sync(ctx) }

// SyncLoop runs Sync every SyncInterval, and whenever now fires, until ctx
// ends.
func (m *Manager) SyncLoop(ctx context.Context, now <-chan struct{}) { m.source.SyncLoop(ctx, now) }

// PodHandler serves the standard daemon protocol to pods, and the sandbox
// service's own pod paths beside it.
func (m *Manager) PodHandler() http.Handler { return m.pool.Handler() }

// AdminHandler serves the read-only admin API (see newAdminHandler).
func (m *Manager) AdminHandler() http.Handler { return newAdminHandler(m) }

// Leases lists active and recently closed leases, oldest first.
func (m *Manager) Leases() []LeaseInfo { return m.core.Leases() }
