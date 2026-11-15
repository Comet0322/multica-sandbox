// Package policy is the pure part of the core's decisions (ADR 0009): which
// tenant claims next and for which runtime kind, where a claimed task goes,
// whether a claim may have a pod, which pod is deleted when the budget is
// full, and when held and idle pods end.
//
// A pod runs one task at a time. It is warm until its first task, then
// belongs to that task's tenant and kind for good:
//
//   - running while it has a task;
//   - held for its task's key, for the tenant's hold timeout, once the task
//     ended (a task with no key is never held);
//   - for the pool kind, idle once its hold ended, until the maximum idle
//     lifetime. A clean pod is deleted instead.
//
// It knows only the pods the caller acquired and has not deleted. It asks for
// a pod with Acquire, learns of one with PodAcquired, and forgets a pod when
// it decides a Delete or hears it was lost.
//
// It is pure and deterministic: no network, no goroutines, no locks, and the
// time comes from the injected clock. Events go in through the Policy methods
// and decisions come out; the caller carries the decisions out. A Policy is
// not safe for concurrent use: the core calls it under its own lock.
//
// It knows nothing of Kubernetes or Multica: a tenant is the unit quota,
// guarantees and keys are scoped to (for Multica, a workspace), and a key is
// an opaque string the caller derives (for Multica, the issue, else the chat
// session).
package policy

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// Kind is a runtime kind: which pods a task may run on.
type Kind string

// The runtime kinds.
const (
	// KindPool tasks may take an idle pod of their tenant that served
	// another key.
	KindPool Kind = "pool"
	// KindClean tasks take their key's held pod or a pod that served no one.
	KindClean Kind = "clean"
)

// Phase is where a pod is in its life.
type Phase string

// The phases of a pod.
const (
	PhaseWarm    Phase = "warm"
	PhaseRunning Phase = "running"
	PhaseHeld    Phase = "held"
	PhaseIdle    Phase = "idle"
)

// Defaults applied to a zero Config field.
const (
	DefaultMaxIdle = 24 * time.Hour
	// DefaultPodWait bounds how long a claimed task whose pod was lost
	// before it started waits for another.
	DefaultPodWait = 3 * time.Minute
	// DefaultRemovedReason is the deletion reason of a removed tenant's
	// pods.
	DefaultRemovedReason = "tenant removed"
)

// Reasons the policy gives.
const (
	reasonHoldExpired = "hold expired"
	reasonIdle        = "idle for its maximum idle lifetime"
	reasonBudget      = "the pod budget is full"
	reasonNoPod       = "no pod could take the task in time"
)

// Config configures a Policy.
type Config struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// HoldTimeout is how long a pod is held for its task's key once the
	// task ended. Zero holds nothing.
	HoldTimeout time.Duration
	// MaxIdle is how long an idle pool pod is kept. Zero means
	// DefaultMaxIdle.
	MaxIdle time.Duration
	// MaxPods is the budget: the most pods, warm, starting, running, held
	// and idle, there may be at once. Zero leaves the budget to the pool:
	// it is full whenever the pool has no pod to give and none is starting.
	MaxPods int
	// Backoff is how long a tenant is not claimed from after its upstream
	// failed a claim.
	Backoff time.Duration
	// PodWait bounds how long a claimed task that lost its pod before it
	// started waits for another before it is failed. Zero means
	// DefaultPodWait.
	PodWait time.Duration
	// RemovedReason is the deletion reason of a removed tenant's pods, in
	// the caller's words. Empty means DefaultRemovedReason.
	RemovedReason string
}

// TenantSettings is what the policy knows of one served tenant.
type TenantSettings struct {
	// Quota caps the pods the tenant's running tasks and claims in flight
	// may occupy at once. Held and idle pods do not count. Zero means no
	// cap.
	Quota int
	// HoldTimeout is the tenant's own hold timeout. Nil means the default.
	HoldTimeout *time.Duration
	// Guarantee is the number of pods kept for the tenant: while it
	// occupies fewer, other tenants may not take the rest (see share.go).
	Guarantee int
	// Weight shares out the pods beyond the guarantees. Zero means one.
	Weight int
}

// Task is what the policy knows of a claimed task.
type Task struct {
	ID     string
	Tenant string
	// Kind is the kind of the runtime the task was claimed for.
	Kind Kind
	// Key is what the task's pod is held for once it ends; empty for none.
	Key string
}

// Target is what one claim is made for: a tenant's runtimes of one kind.
type Target struct {
	Tenant string
	Kind   Kind
}

type podState struct {
	id  string
	seq uint64
	// tenants are the tenants the pod's daemon registered for; it is given
	// only their tasks.
	tenants map[string]bool
	phase   Phase
	// tenant, kind and key are set by the pod's first task; key changes
	// with each task.
	tenant      string
	kind        Kind
	key         string
	task        *taskState
	reservation *reservation
	// since is when the pod was held or became idle; until is when its
	// hold ends.
	since, until time.Time
}

type taskState struct {
	Task
	started bool
	pod     *podState
	// waitingSince is set while the task waits for a pod.
	waitingSince time.Time
}

type reservation struct {
	id     ReservationID
	pod    *podState
	target Target
}

// Policy is the policy's state. See the package comment.
type Policy struct {
	cfg          Config
	pods         map[string]*podState
	seq          uint64
	tenants      map[string]TenantSettings
	backoff      map[string]time.Time
	pending      map[Target]*pendingList
	tasks        map[string]*taskState
	waiting      []*taskState
	reservations map[ReservationID]*reservation
	// lastTarget is the target claimed for last; targets claimed for
	// without a queued task take turns after it.
	lastTarget Target
	// livePods is the number of live pods, as the latest claim request
	// said.
	livePods int
	nextRes  ReservationID
}

// New returns a policy with no pods and no tenants.
func New(cfg Config) (*Policy, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HoldTimeout < 0 || cfg.MaxIdle < 0 || cfg.MaxPods < 0 || cfg.Backoff < 0 || cfg.PodWait < 0 {
		return nil, fmt.Errorf("hold timeout, idle lifetime, budget, backoff and pod wait must not be negative")
	}
	if cfg.MaxIdle == 0 {
		cfg.MaxIdle = DefaultMaxIdle
	}
	if cfg.PodWait == 0 {
		cfg.PodWait = DefaultPodWait
	}
	if cfg.RemovedReason == "" {
		cfg.RemovedReason = DefaultRemovedReason
	}
	return &Policy{
		cfg: cfg, pods: map[string]*podState{}, tenants: map[string]TenantSettings{},
		backoff: map[string]time.Time{}, pending: map[Target]*pendingList{}, tasks: map[string]*taskState{},
		reservations: map[ReservationID]*reservation{},
	}, nil
}

// SetTenant adds a served tenant or changes its settings. The settings
// apply from now on.
func (p *Policy) SetTenant(id string, s TenantSettings) error {
	if s.Quota < 0 || s.Guarantee < 0 || s.Weight < 0 {
		return fmt.Errorf("tenant %s: quota, guarantee and weight must not be negative", id)
	}
	if s.HoldTimeout != nil && *s.HoldTimeout < 0 {
		return fmt.Errorf("tenant %s: hold timeout must not be negative", id)
	}
	p.tenants[id] = s
	return nil
}

// RemoveTenant stops claiming from a tenant. Its tasks run on; its held and
// idle pods, which nothing can use any more, are deleted, and so is each of
// its running pods when its task ends.
func (p *Policy) RemoveTenant(id string) []Decision {
	delete(p.tenants, id)
	delete(p.backoff, id)
	for tg := range p.pending {
		if tg.Tenant == id {
			delete(p.pending, tg)
		}
	}
	var ds []Decision
	for _, pd := range p.sortedPods() {
		if pd.tenant == id && (pd.phase == PhaseHeld || pd.phase == PhaseIdle) {
			ds = append(ds, p.delete(pd, Delete{Pod: pd.id, Reason: p.cfg.RemovedReason}))
		}
	}
	return ds
}

// PodAcquired records a ready pod the caller acquired, warm, with the
// tenants its daemon registered for. Tasks waiting for a pod are placed
// first.
func (p *Policy) PodAcquired(pod string, tenants ...string) []Decision {
	pd := p.pods[pod]
	if pd == nil {
		p.seq++
		pd = &podState{id: pod, seq: p.seq, tenants: map[string]bool{}, phase: PhaseWarm}
		p.pods[pod] = pd
	}
	for _, t := range tenants {
		pd.tenants[t] = true
	}
	return p.placeWaiting()
}

// PodRestored records a pod a previous run of the caller left, in the phase
// it was in: a running pod's task is started and runs on, a hold ends at its
// own end and an idle pod ages from when it became idle. The pod serves its
// own tenant only. A warm one is PodAcquired with no tenant yet.
func (p *Policy) PodRestored(pod string, info PodInfo) []Decision {
	if info.Phase == PhaseWarm || info.Phase == "" {
		return p.PodAcquired(pod)
	}
	p.seq++
	pd := &podState{id: pod, seq: p.seq, tenants: map[string]bool{info.Tenant: true}, phase: info.Phase,
		tenant: info.Tenant, kind: info.Kind, key: info.Key, since: info.Since, until: info.Until}
	p.pods[pod] = pd
	if info.Phase == PhaseRunning {
		ts := &taskState{Task: Task{ID: info.Task, Tenant: info.Tenant, Kind: info.Kind, Key: info.Key}, started: true, pod: pd}
		pd.task = ts
		p.tasks[ts.ID] = ts
	}
	return p.placeWaiting()
}

// PodRegistered records that the daemon of a held pod registered for a
// tenant. A pod the policy does not hold is ignored.
func (p *Policy) PodRegistered(pod, tenant string) []Decision {
	pd := p.pods[pod]
	if pd == nil {
		return nil
	}
	pd.tenants[tenant] = true
	return p.placeWaiting()
}

// PodLost records that a held pod is gone. A started task on it is failed
// with failReason and its lease ended with endReason; a task placed on it
// that it had not picked up waits for another pod. The pod is forgotten.
func (p *Policy) PodLost(pod, failReason, endReason string) []Decision {
	pd := p.pods[pod]
	if pd == nil {
		return nil
	}
	if r := pd.reservation; r != nil {
		delete(p.reservations, r.id)
	}
	delete(p.pods, pod)
	var ds []Decision
	if ts := pd.task; ts != nil {
		ts.pod = nil
		if ts.started {
			delete(p.tasks, ts.ID)
			ds = append(ds, Fail{Task: ts.ID, Tenant: ts.Tenant, Reason: failReason}, EndLease{Task: ts.ID, Reason: endReason})
		} else {
			p.wait(ts)
		}
	}
	return append(ds, p.placeWaiting()...)
}

// PodInterrupted is PodLost: a pod whose daemon restarted ran its task no
// further.
func (p *Policy) PodInterrupted(pod, failReason, endReason string) []Decision {
	return p.PodLost(pod, failReason, endReason)
}

// TaskStarted records that the pod picked up a task placed on it.
func (p *Policy) TaskStarted(id string) {
	if ts := p.tasks[id]; ts != nil {
		ts.started = true
	}
}

// TaskEnded records that a task is over. Its lease ends, and its pod is held
// for the task's key; with no key or no hold timeout, a pool pod becomes
// idle and a clean pod is deleted. A removed tenant's pod is deleted.
func (p *Policy) TaskEnded(id, reason string) []Decision {
	ds := []Decision{EndLease{Task: id, Reason: reason}}
	ts := p.tasks[id]
	if ts == nil {
		return ds
	}
	delete(p.tasks, id)
	p.waiting = slices.DeleteFunc(p.waiting, func(w *taskState) bool { return w == ts })
	pd := ts.pod
	if pd == nil {
		return append(ds, p.placeWaiting()...)
	}
	pd.task = nil
	now := p.cfg.Now()
	_, served := p.tenants[pd.tenant]
	switch hold := p.holdTimeout(pd.tenant); {
	case !served:
		ds = append(ds, p.delete(pd, Delete{Pod: pd.id, Reason: p.cfg.RemovedReason, Task: id}))
	case ts.Key != "" && hold > 0:
		pd.phase, pd.since, pd.until = PhaseHeld, now, now.Add(hold)
	case pd.kind == KindClean:
		ds = append(ds, p.delete(pd, Delete{Pod: pd.id, Reason: "task ended: " + reason, Task: id}))
	default:
		p.idle(pd, now)
	}
	return append(ds, p.placeWaiting()...)
}

// Tick lets time pass: held pods whose hold ended become idle (pool) or are
// deleted (clean), idle pods past the maximum idle lifetime are deleted, and
// tasks that waited too long for a pod are failed.
func (p *Policy) Tick() []Decision {
	ds := p.placeWaiting()
	now := p.cfg.Now()
	for _, pd := range p.sortedPods() {
		if pd.reservation != nil {
			continue // left to the claim's outcome
		}
		if pd.phase == PhaseHeld && !now.Before(pd.until) {
			if pd.kind == KindClean {
				ds = append(ds, p.delete(pd, Delete{Pod: pd.id, Reason: reasonHoldExpired}))
				continue
			}
			p.idle(pd, pd.until)
		}
		if pd.phase == PhaseIdle && !now.Before(pd.since.Add(p.cfg.MaxIdle)) {
			ds = append(ds, p.delete(pd, Delete{Pod: pd.id, Reason: reasonIdle}))
		}
	}
	still := p.waiting[:0]
	for _, ts := range p.waiting {
		if now.Sub(ts.waitingSince) > p.cfg.PodWait {
			delete(p.tasks, ts.ID)
			ds = append(ds, Fail{Task: ts.ID, Tenant: ts.Tenant, Reason: reasonNoPod})
			continue
		}
		still = append(still, ts)
	}
	p.waiting = still
	return append(ds, p.placeWaiting()...)
}

// Busy reports whether a tenant still has tasks or claims in flight here.
func (p *Policy) Busy(tenant string) bool {
	for _, ts := range p.tasks {
		if ts.Tenant == tenant {
			return true
		}
	}
	for _, r := range p.reservations {
		if r.target.Tenant == tenant {
			return true
		}
	}
	return false
}

// PodInfo describes a pod the policy holds.
type PodInfo struct {
	Phase Phase
	// Tenant and Kind are empty while the pod is warm.
	Tenant string
	Kind   Kind
	// Key is the key of the pod's task, or the key it is held for.
	Key string
	// Task is the task placed on or running on the pod.
	Task string
	// Reserved is set while a claim in flight was made against the pod.
	Reserved bool
	// Since is when a held pod was held or an idle pod became idle; Until
	// is when a hold ends.
	Since, Until time.Time
}

// Pod describes a pod the policy holds; false when it holds none by that ID.
func (p *Policy) Pod(id string) (PodInfo, bool) {
	pd := p.pods[id]
	if pd == nil {
		return PodInfo{}, false
	}
	info := PodInfo{Phase: pd.phase, Tenant: pd.tenant, Kind: pd.kind, Key: pd.key, Reserved: pd.reservation != nil, Since: pd.since, Until: pd.until}
	if pd.task != nil {
		info.Task = pd.task.ID
	}
	return info, true
}

func (p *Policy) holdTimeout(tenant string) time.Duration {
	if s, ok := p.tenants[tenant]; ok && s.HoldTimeout != nil {
		return *s.HoldTimeout
	}
	return p.cfg.HoldTimeout
}

func (p *Policy) idle(pd *podState, since time.Time) {
	pd.phase, pd.key, pd.since, pd.until = PhaseIdle, "", since, time.Time{}
}

// delete forgets a pod the caller is to delete.
func (p *Policy) delete(pd *podState, d Delete) Decision {
	if r := pd.reservation; r != nil {
		delete(p.reservations, r.id)
	}
	delete(p.pods, pd.id)
	return d
}

func (p *Policy) sortedPods() []*podState {
	out := make([]*podState, 0, len(p.pods))
	for _, pd := range p.pods {
		out = append(out, pd)
	}
	slices.SortFunc(out, func(a, b *podState) int { return cmp.Compare(a.id, b.id) })
	return out
}

// Placement.

// placeFor is the pod a task goes to, nil when none can take it now: its
// key's held pod, then (pool) an idle pod of its tenant, then a warm pod.
// reserved, the pod readied for the task's claim, is preferred among equals.
func (p *Policy) placeFor(ts *taskState, reserved *podState) *podState {
	free := func(pd *podState) bool {
		return pd.tenants[ts.Tenant] && (pd.reservation == nil || pd == reserved)
	}
	if ts.Key != "" {
		for _, pd := range p.sortedPods() {
			if pd.phase == PhaseHeld && pd.tenant == ts.Tenant && pd.kind == ts.Kind && pd.key == ts.Key && free(pd) {
				return pd
			}
		}
	}
	if ts.Kind == KindPool {
		if pd := p.idlePod(ts.Tenant, reserved); pd != nil {
			return pd
		}
	}
	return p.warmPod(ts.Tenant, reserved)
}

// idlePod is an idle pod of the tenant no other claim was made against:
// reserved when it is one, else the one idle the shortest time, so the
// others age out.
func (p *Policy) idlePod(tenant string, reserved *podState) *podState {
	var best *podState
	for _, pd := range p.sortedPods() {
		if pd.phase != PhaseIdle || pd.tenant != tenant || !pd.tenants[tenant] || (pd.reservation != nil && pd != reserved) {
			continue
		}
		if pd == reserved {
			return pd
		}
		if best == nil || pd.since.After(best.since) {
			best = pd
		}
	}
	return best
}

// warmPod is a warm pod registered for the tenant that no other claim was
// made against: reserved when it is one, else the earliest acquired.
func (p *Policy) warmPod(tenant string, reserved *podState) *podState {
	var best *podState
	for _, pd := range p.pods {
		if pd.phase != PhaseWarm || !pd.tenants[tenant] || (pd.reservation != nil && pd != reserved) {
			continue
		}
		if pd == reserved {
			return pd
		}
		if best == nil || pd.seq < best.seq {
			best = pd
		}
	}
	return best
}

// put places a task on a pod, which runs it from now on.
func (p *Policy) put(pd *podState, ts *taskState) Decision {
	pd.phase, pd.task, pd.since, pd.until = PhaseRunning, ts, time.Time{}, time.Time{}
	pd.tenant, pd.kind, pd.key = ts.Tenant, ts.Kind, ts.Key
	ts.pod, ts.waitingSince = pd, time.Time{}
	p.tasks[ts.ID] = ts
	return Place{Task: ts.ID, Tenant: ts.Tenant, Pod: pd.id}
}

// wait makes a task wait for a pod, from now.
func (p *Policy) wait(ts *taskState) {
	ts.pod = nil
	ts.waitingSince = p.cfg.Now()
	p.tasks[ts.ID] = ts
	p.waiting = append(p.waiting, ts)
}

// placeWaiting places the waiting tasks that a pod can take now, oldest
// first.
func (p *Policy) placeWaiting() []Decision {
	if len(p.waiting) == 0 {
		return nil
	}
	slices.SortStableFunc(p.waiting, func(a, b *taskState) int { return a.waitingSince.Compare(b.waitingSince) })
	var ds []Decision
	still := p.waiting[:0]
	for _, ts := range p.waiting {
		if pd := p.placeFor(ts, nil); pd != nil {
			ds = append(ds, p.put(pd, ts))
			continue
		}
		still = append(still, ts)
	}
	p.waiting = still
	return ds
}
