package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PodState is where a pod stands for callers.
type PodState string

const (
	// PodFree: registered, live and reset; Create may return it.
	PodFree PodState = "free"
	// PodHeld: a caller acquired it with Create and has not handed it back.
	PodHeld PodState = "held"
	// PodResetting: its supervisor was asked to reset it and has not reported
	// the reset done.
	PodResetting PodState = "resetting"
	// PodLost: it went silent, or its reset was not reported done in time.
	PodLost PodState = "lost"
)

// DefaultHeartbeatTimeout is how long a pod may stay silent on the control
// channel before it is lost.
const DefaultHeartbeatTimeout = 45 * time.Second

// DefaultResetTimeout bounds one reset: from the command until the supervisor
// reports the daemon home cleared.
const DefaultResetTimeout = 2 * time.Minute

// DefaultLostRetention is how long a lost pod stays listed before it is pruned.
const DefaultLostRetention = 10 * time.Minute

// sweepInterval is how often Run checks liveness, resets and retention.
const sweepInterval = time.Second

// Config configures a GitOpsService.
type Config struct {
	// HeartbeatTimeout is how long a pod may stay silent on the control
	// channel before it is lost. Zero means DefaultHeartbeatTimeout.
	HeartbeatTimeout time.Duration
	// ResetTimeout is how long a reset may go unreported before the pod is
	// lost. Zero means DefaultResetTimeout.
	ResetTimeout time.Duration
	// LostRetention is how long a lost pod stays listed before it is pruned
	// and its token refused. Zero means DefaultLostRetention.
	LostRetention time.Duration
	// Notify is told when a held pod is interrupted or lost. Nil tells no one.
	Notify Notifier
	// Audit takes the reset audit records. Nil records nothing.
	Audit AuditWriter
	// Log is the service's logger. Nil means slog.Default().
	Log *slog.Logger
	// Now is the time source for timestamps and timeouts. Nil means time.Now.
	Now func() time.Time
	// Monotonic is the process's own clock, used only to tell a stall of
	// this process from pods that went silent. Nil means time.Now.
	Monotonic func() time.Time
}

func (c *Config) normalize() error {
	for name, d := range map[string]*time.Duration{
		"heartbeat timeout": &c.HeartbeatTimeout, "reset timeout": &c.ResetTimeout, "lost retention": &c.LostRetention,
	} {
		if *d < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if c.HeartbeatTimeout == 0 {
		c.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if c.ResetTimeout == 0 {
		c.ResetTimeout = DefaultResetTimeout
	}
	if c.LostRetention == 0 {
		c.LostRetention = DefaultLostRetention
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Monotonic == nil {
		c.Monotonic = time.Now
	}
	return nil
}

// GitOpsService is the sandbox service for pods that start on their own
// (GitOps) and register themselves. It is not the manager's Pool (see
// GLOSSARY.md).
type GitOpsService struct {
	cfg Config
	log *slog.Logger

	mu   sync.Mutex
	pods map[PodID]*pod
	// tokens maps each issued pod token to its pod.
	tokens map[string]PodID
	// seq numbers registrations, for stable listings.
	seq uint64
	// prunedTotal counts lost pods dropped after the retention period.
	prunedTotal int
	// lastSweep is when sweepLocked last ran, on the Monotonic clock.
	lastSweep time.Time
	// notes are the hook calls due once mu is released.
	notes []func()
}

type pod struct {
	id PodID
	// label is the name the pod gave itself (such as its Kubernetes pod
	// name), shown only.
	label        string
	seq          uint64
	state        PodState
	registeredAt time.Time
	lastSeen     time.Time
	// lostAt is when the pod was lost; the retention runs from it.
	lostAt time.Time
	// resetID names the reset command the supervisor is asked to run;
	// resetStarted starts the reset timeout; resetLabel is the cause the
	// caller named, for the audit trail.
	resetID      string
	resetStarted time.Time
	resetLabel   AuditLabel
}

// NewGitOps returns a sandbox service with no pods.
func NewGitOps(cfg Config) (*GitOpsService, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &GitOpsService{cfg: cfg, log: cfg.Log, pods: map[PodID]*pod{}, tokens: map[string]PodID{}}, nil
}

func (p *GitOpsService) now() time.Time { return p.cfg.Now() }

// unlock releases mu, then makes the hook calls queued meanwhile.
func (p *GitOpsService) unlock() {
	notes := p.notes
	p.notes = nil
	p.mu.Unlock()
	for _, n := range notes {
		n()
	}
}

// Run checks liveness, resets and retention every second until ctx ends.
// Every call into the service checks them too.
func (p *GitOpsService) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			p.sweepLocked()
			p.unlock()
		}
	}
}

// podTokenPrefix marks a pod token, so it is never mistaken for a task token
// ("mat_") or a PAT.
const podTokenPrefix = "mpt_"

// registerPod gives a container that started a fresh identity and its token.
// It is reset before it is ever acquired, so its supervisor is known to
// answer.
func (p *GitOpsService) registerPod(label string) (PodID, string) {
	p.mu.Lock()
	defer p.unlock()
	p.sweepLocked()
	now := p.now()
	pd := &pod{label: label, registeredAt: now, lastSeen: now}
	for {
		pd.id = PodID("pod-" + randomHex(6))
		if p.pods[pd.id] == nil {
			break
		}
	}
	token := podTokenPrefix + randomHex(32)
	p.seq++
	pd.seq = p.seq
	p.pods[pd.id], p.tokens[token] = pd, pd.id
	p.startResetLocked(pd, "pod registered; it is reset before it is first lent", AuditLabel{})
	p.log.Info("pod registered", "pod_id", pd.id, "name", label)
	return pd.id, token
}

// Authenticate returns the pod that holds the pod token. A pruned pod's token
// is refused.
func (p *GitOpsService) Authenticate(token string) (PodID, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.tokens[token]
	return id, ok
}

// Create acquires a free pod, the earliest registered first.
func (p *GitOpsService) Create(ctx context.Context) (PodID, error) {
	p.mu.Lock()
	defer p.unlock()
	p.sweepLocked()
	var pick *pod
	for _, pd := range p.pods {
		if pd.state == PodFree && (pick == nil || pd.seq < pick.seq) {
			pick = pd
		}
	}
	if pick == nil {
		return "", ErrNoCapacity
	}
	pick.state = PodHeld
	return pick.id, nil
}

// Delete hands a pod back: its reset starts at once, and it is free again
// once its supervisor reports the reset done.
func (p *GitOpsService) Delete(ctx context.Context, id PodID, label AuditLabel) error {
	p.mu.Lock()
	defer p.unlock()
	p.sweepLocked()
	pd := p.pods[id]
	if pd == nil || pd.state == PodLost || pd.state == PodResetting {
		return nil
	}
	p.startResetLocked(pd, label.Reason, label)
	return nil
}

// startResetLocked issues a new reset command for the pod's supervisor and
// audits its start under the cause the label names (empty for a reset no
// caller asked for). Callers hold p.mu.
func (p *GitOpsService) startResetLocked(pd *pod, reason string, label AuditLabel) {
	pd.state = PodResetting
	pd.resetID = uuid.NewString()
	pd.resetStarted = p.now()
	pd.resetLabel = label
	p.log.Info("pod reset started", "pod_id", pd.id, "reset_id", pd.resetID, "reason", reason)
	p.auditReset(pd, AuditAllow, "reset started: "+reason)
}

// sweepLocked loses every pod whose control channel went silent past the
// heartbeat timeout, and every pod whose reset was not reported done within
// the reset timeout, then prunes lost pods past their retention. A resetting
// pod may be slow to report, so only the reset timeout applies to it.
// Callers hold p.mu.
//
// Supervisors poll every 2 seconds and callers reach the service at least
// every second, so a gap longer than the heartbeat timeout since the last
// sweep means this process itself stalled (CPU throttling, a paused process)
// rather than every pod at once: the pods get one grace period instead of all
// being lost. The gap is measured on the Monotonic clock, never less than
// DefaultHeartbeatTimeout.
func (p *GitOpsService) sweepLocked() {
	now := p.now()
	mono := p.cfg.Monotonic()
	if stall := max(p.cfg.HeartbeatTimeout, DefaultHeartbeatTimeout); !p.lastSweep.IsZero() && mono.Sub(p.lastSweep) > stall {
		p.log.Warn("sandbox service stalled longer than the heartbeat timeout; giving every pod one grace period", "stalled_for", mono.Sub(p.lastSweep))
		for _, pd := range p.pods {
			if pd.state != PodLost {
				pd.lastSeen = now
			}
		}
	}
	p.lastSweep = mono
	p.pruneLocked(now)
	for _, pd := range p.pods {
		switch {
		case pd.state == PodResetting:
			if now.Sub(pd.resetStarted) > p.cfg.ResetTimeout {
				p.log.Warn("pod lost: its reset was not reported done in time", "pod_id", pd.id, "name", pd.label, "reset_started", pd.resetStarted)
				p.auditReset(pd, AuditDeny, "reset timed out: the supervisor did not report it done in time")
				p.loseLocked(pd, "reset timed out")
			}
		case pd.state != PodLost && now.Sub(pd.lastSeen) > p.cfg.HeartbeatTimeout:
			p.log.Warn("pod lost: heartbeat timed out", "pod_id", pd.id, "name", pd.label, "last_seen", pd.lastSeen)
			pd.resetLabel = AuditLabel{}
			p.auditReset(pd, AuditDeny, "pod lost: heartbeat timed out")
			p.loseLocked(pd, "pod lost")
		}
	}
}

// loseLocked marks the pod lost. Silence says nothing of what the pod did
// meanwhile, so if it comes back it is reset before it is acquired again.
// A caller holding it is told. Callers hold p.mu.
func (p *GitOpsService) loseLocked(pd *pod, why string) {
	held := pd.state == PodHeld
	pd.state = PodLost
	pd.lostAt = p.now()
	pd.resetID = ""
	if held {
		p.notifyLocked(func(n Notifier) { n.Lost(pd.id, why) })
	}
}

// notifyLocked queues a hook call for when mu is released. Callers hold p.mu.
func (p *GitOpsService) notifyLocked(call func(Notifier)) {
	if n := p.cfg.Notify; n != nil {
		p.notes = append(p.notes, func() { call(n) })
	}
}

// pruneLocked drops the pods lost for longer than the retention, with their
// tokens, which are refused from then on. Callers hold p.mu.
func (p *GitOpsService) pruneLocked(now time.Time) {
	pruned := false
	for id, pd := range p.pods {
		if pd.state == PodLost && now.Sub(pd.lostAt) > p.cfg.LostRetention {
			p.log.Info("lost pod pruned", "pod_id", pd.id, "name", pd.label, "lost_at", pd.lostAt)
			delete(p.pods, id)
			p.prunedTotal++
			pruned = true
		}
	}
	if pruned {
		for token, id := range p.tokens {
			if p.pods[id] == nil {
				delete(p.tokens, token)
			}
		}
	}
}

// Pod reports one pod as of the latest sweep: a plain lookup, cheaper than
// Inventory. It does not sweep, so it is safe to call while holding a lock
// that a notification takes (see Notifier).
func (p *GitOpsService) Pod(id PodID) (PodInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pods[id]
	if pd == nil {
		return PodInfo{}, false
	}
	return pd.info(), true
}

func (pd *pod) info() PodInfo {
	return PodInfo{ID: pd.id, Label: pd.label, State: pd.state, RegisteredAt: pd.registeredAt, LastSeen: pd.lastSeen}
}

// PodInfo is one pod as the inventory lists it.
type PodInfo struct {
	ID           PodID
	Label        string
	State        PodState
	RegisteredAt time.Time
	LastSeen     time.Time
}

// Inventory lists the pods in registration order.
type Inventory struct {
	Pods []PodInfo
	// PrunedTotal counts the lost pods pruned since the service started.
	PrunedTotal int
}

// Inventory reports every pod and how many were pruned.
func (p *GitOpsService) Inventory() Inventory {
	p.mu.Lock()
	defer p.unlock()
	p.sweepLocked()
	list := make([]*pod, 0, len(p.pods))
	for _, pd := range p.pods {
		list = append(list, pd)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seq < list[j].seq })
	inv := Inventory{Pods: make([]PodInfo, 0, len(list)), PrunedTotal: p.prunedTotal}
	for _, pd := range list {
		inv.Pods = append(inv.Pods, pd.info())
	}
	return inv
}

// errUnknownPod is a control call whose pod token names no pod (any more).
var errUnknownPod = errors.New("unknown pod token")

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
