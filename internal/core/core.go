package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"sandbox-manager/internal/policy"
)

// Defaults the core applies to a zero Config field.
const (
	// DefaultDispatchInterval is the dispatch tick when none is set.
	DefaultDispatchInterval = time.Second
	// DefaultLeaseMax bounds a lease when no maximum lease time is set.
	DefaultLeaseMax = 2 * time.Hour
	// DefaultDrainTimeout is how long a removed tenant may keep its leases.
	DefaultDrainTimeout = 10 * time.Minute
)

// Config configures a Core. The policy settings are passed to the policy
// (see policy.Config).
type Config struct {
	Log   *slog.Logger
	Clock *Clock
	// Audit receives the audit records; nil writes none.
	Audit *AuditLog

	// Kinds are the runtime kinds claimed for. Empty means the pool kind.
	Kinds         []policy.Kind
	RemovedReason string
	HoldTimeout   time.Duration
	MaxIdle       time.Duration
	MaxPods       int
	Backoff       time.Duration
	PodWait       time.Duration

	// DispatchInterval is the dispatch tick, the only place tasks are
	// claimed. Zero means DefaultDispatchInterval.
	DispatchInterval time.Duration
	// ReconcileInterval turns on lease reconciliation; zero turns it off.
	ReconcileInterval time.Duration
	// LeaseMax is the longest a lease may last. Zero means DefaultLeaseMax.
	LeaseMax time.Duration
	// DrainTimeout is how long a removed tenant may keep its leases before
	// they are ended; DrainPoll how often it is checked. Zero means
	// DefaultDrainTimeout and one second.
	DrainTimeout time.Duration
	DrainPoll    time.Duration
}

// Core lends pods to tasks. It owns the dispatch state (the policy,
// the held pods, the dispatch record of every task, the tenants) under mu,
// and calls the TaskSource and the Pool only without it, except for the
// Pool's methods that answer from its own state.
type Core struct {
	cfg    Config
	log    *slog.Logger
	clock  *Clock
	source TaskSource
	pool   Pool

	mu sync.Mutex
	// policy is the policy; every placement, claim-selection and
	// lifetime decision comes from it.
	policy *policy.Policy
	// held are the pods acquired from the pool and not handed back, by pod
	// ID; heldSeq numbers their acquisitions.
	held    map[string]*pod
	heldSeq uint64
	// handBacks are the pods the policy handed back, not yet passed to the
	// pool.
	handBacks []handBack
	// leases holds the active leases by task ID; history keeps them, closed
	// ones included, for the lease query.
	leases  map[string]*lease
	history []*lease
	// claimed holds, by task ID, the tasks claimed that no pod has picked
	// up yet.
	claimed map[string]*claimedTask
	// forced holds tasks the policy decided to fail, not yet reported.
	forced []forcedTask
	// tenants are the tenants served or draining, by ID.
	tenants map[string]*tenant
	// recorded is what the Pool was last told each held pod serves.
	recorded map[string]PodRecord
}

// New builds the core. Connect gives it its sides before it serves anything.
func New(cfg Config) (*Core, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = NewClock()
	}
	if cfg.DispatchInterval < 0 || cfg.ReconcileInterval < 0 || cfg.LeaseMax < 0 || cfg.DrainTimeout < 0 || cfg.DrainPoll < 0 {
		return nil, fmt.Errorf("intervals and timeouts must not be negative")
	}
	if cfg.DispatchInterval == 0 {
		cfg.DispatchInterval = DefaultDispatchInterval
	}
	cfg.Kinds = policy.KindsOrPool(cfg.Kinds)
	if cfg.LeaseMax == 0 {
		cfg.LeaseMax = DefaultLeaseMax
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if cfg.DrainPoll == 0 {
		cfg.DrainPoll = time.Second
	}
	c := &Core{cfg: cfg, log: cfg.Log, clock: cfg.Clock, held: map[string]*pod{},
		leases: map[string]*lease{}, claimed: map[string]*claimedTask{}, tenants: map[string]*tenant{},
		recorded: map[string]PodRecord{}}
	pol, err := policy.New(policy.Config{
		Now:           c.now,
		RemovedReason: cfg.RemovedReason,
		HoldTimeout:   cfg.HoldTimeout,
		MaxIdle:       cfg.MaxIdle,
		MaxPods:       cfg.MaxPods,
		Backoff:       cfg.Backoff,
		PodWait:       cfg.PodWait,
	})
	if err != nil {
		return nil, err
	}
	c.policy = pol
	return c, nil
}

// Connect gives the core its task source and its pool.
func (c *Core) Connect(source TaskSource, pool Pool) {
	c.source, c.pool = source, pool
}

func (c *Core) now() time.Time { return c.clock.Now() }

// tickLocked lets time pass for the policy (holds, idle lifetimes,
// tasks waiting for a pod) and ends leases past the maximum lease time. Callers hold
// c.mu, and settle once they release it.
func (c *Core) tickLocked() {
	c.applyLocked(c.policy.Tick())
	c.expireLeasesLocked()
}

// Touch lets time pass for the policy and the leases.
func (c *Core) Touch() {
	c.mu.Lock()
	c.tickLocked()
	c.mu.Unlock()
	c.settle(context.Background())
}

// Run runs the dispatch tick until ctx ends; with ReconcileInterval set it
// also runs the lease reconciliation.
func (c *Core) Run(ctx context.Context) {
	if c.cfg.ReconcileInterval > 0 {
		go c.reconcileLoop(ctx)
	}
	t := time.NewTicker(c.cfg.DispatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.DispatchOnce(ctx)
		}
	}
}
