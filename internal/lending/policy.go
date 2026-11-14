// Package lending is the sandbox manager's lending policy: which workspace to
// claim from, which pod a claimed task runs on, which tasks share a pod, and
// when a pod is handed back to be reset or a task failed.
//
// It knows only the pods the manager holds: those it acquired from the
// sandbox service and has not handed back. It asks for a pod with Acquire,
// learns of one with PodAcquired, and forgets a pod when it decides a Reset
// (the hand-back) or hears it was lost or interrupted.
//
// It is pure and deterministic: no network, no goroutines, no locks, and the
// time comes from the injected clock. Events go in through the Policy methods
// and decisions come out; the manager carries the decisions out. A Policy is
// not safe for concurrent use: the manager calls it under its own lock.
package lending

import (
	"fmt"
	"slices"
	"time"
)

// Config configures a Policy.
type Config struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Capacity is what every pod offers. It is pool-wide.
	Capacity Resources
	// Rule is the default affinity rule, a name in Rules. Empty means
	// RulePersonIssue.
	Rule AffinityRule
	// HoldTimeout is how long a binding with no running task keeps its pod
	// for its affinity key before the pod is reset. Zero resets the pod as
	// soon as its last task ends.
	HoldTimeout time.Duration
	// MaxLifetime is how long a binding takes new tasks after it opened. Past
	// it, its pod is reset once its tasks end. Zero means no maximum. A
	// binding whose key is unique to its task has none: it ends with its task.
	MaxLifetime time.Duration
	// DrainGrace is how long the tasks of a binding past its maximum lifetime
	// may still run before they are failed and the pod reset.
	DrainGrace time.Duration
	// Rules are the affinity rules workspaces may use. Nil means Builtin().
	Rules Registry
	// Backoff is how long a workspace is not claimed from after its upstream
	// failed a claim.
	Backoff time.Duration
	// PlacementWait bounds how long a claimed task that lost its pod before
	// starting (see PodLost) waits for another before it is failed. Zero means
	// DefaultPlacementWait.
	PlacementWait time.Duration
}

// WorkspaceSettings is what the policy knows of one served workspace.
type WorkspaceSettings struct {
	// Quota caps the capacity the workspace's tasks and claims in flight may
	// occupy at once, in slots. Zero means no cap.
	Quota int
	// Rule is the workspace's affinity rule. Empty means the pool default.
	Rule AffinityRule
	// HoldTimeout is the hold timeout of the bindings the workspace's tasks
	// open. Nil means the pool default.
	HoldTimeout *time.Duration
	// MaxLifetime is the maximum lifetime of the bindings the workspace's
	// tasks open from now on; a binding keeps the lifetime it opened with.
	// Nil means the pool default, and zero means none.
	MaxLifetime *time.Duration
}

// podState is a pod the manager holds: one it acquired from the sandbox
// service and has not handed back.
type podState struct {
	id string
	// workspaces are the workspaces the pod's daemon registered for; the pod
	// is lent only to them.
	workspaces  map[string]bool
	binding     *binding
	reservation *reservation
}

// bindingState is the part of its lifecycle a binding is in: running while it
// has tasks, then in hold until a task of its key renews it or the hold runs
// out and its pod is reset.
type bindingState int

const (
	bindingRunning bindingState = iota
	bindingHold
)

type binding struct {
	id        BindingID
	key       string
	workspace string
	pod       *podState
	state     bindingState
	tasks     map[string]*taskState
	used      Resources
	openedAt  time.Time
	// lifetime is the maximum lifetime the binding opened with; zero is none.
	lifetime  time.Duration
	lastUsed  time.Time
	heldSince time.Time
	holdUntil time.Time
}

type taskState struct {
	Task
	key     string
	started bool
	binding *binding
	// waitingSince is set while the task waits for a pod after losing its own.
	waitingSince time.Time
	// evictedFor is set once a hold was ended for the waiting task: the pod
	// handed back is on its way, so no second hold is ended for it.
	evictedFor bool
}

type reservation struct {
	id        ReservationID
	pod       *podState
	workspace string
}

// Policy is the lending policy's state. See the package comment.
type Policy struct {
	cfg          Config
	pods         map[string]*podState
	workspaces   map[string]WorkspaceSettings
	backoff      map[string]time.Time
	pending      map[string]*pendingList
	tasks        map[string]*taskState
	waiting      []*taskState
	reservations map[ReservationID]*reservation
	// lastWorkspace is the workspace claimed from last; workspaces claimed
	// from without a queued task take turns after it.
	lastWorkspace string
	nextRes       ReservationID
	nextBinding   BindingID
}

// New returns a policy with no pods and no workspaces.
func New(cfg Config) (*Policy, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rules == nil {
		cfg.Rules = Builtin()
	}
	if cfg.Rule == "" {
		cfg.Rule = RulePersonIssue
	}
	if cfg.HoldTimeout < 0 || cfg.MaxLifetime < 0 || cfg.DrainGrace < 0 {
		return nil, fmt.Errorf("hold timeout, maximum lifetime and drain grace must not be negative")
	}
	if cfg.Rules[cfg.Rule] == nil {
		return nil, fmt.Errorf("unknown affinity rule %q", cfg.Rule)
	}
	if !cfg.Capacity.fits(TaskDemand) {
		return nil, fmt.Errorf("pod capacity must fit at least one task")
	}
	if cfg.Backoff < 0 || cfg.PlacementWait < 0 {
		return nil, fmt.Errorf("backoff and placement wait must not be negative")
	}
	if cfg.PlacementWait == 0 {
		cfg.PlacementWait = DefaultPlacementWait
	}
	return &Policy{
		cfg: cfg, pods: map[string]*podState{}, workspaces: map[string]WorkspaceSettings{},
		backoff: map[string]time.Time{}, pending: map[string]*pendingList{}, tasks: map[string]*taskState{}, reservations: map[ReservationID]*reservation{},
	}, nil
}

// SetWorkspace adds a served workspace or changes its settings. The settings
// apply to claims and placements from now on.
func (p *Policy) SetWorkspace(id string, s WorkspaceSettings) error {
	if s.Quota < 0 {
		return fmt.Errorf("workspace %s: quota must not be negative", id)
	}
	if s.Rule != "" && p.cfg.Rules[s.Rule] == nil {
		return fmt.Errorf("workspace %s: unknown affinity rule %q", id, s.Rule)
	}
	if s.HoldTimeout != nil && *s.HoldTimeout < 0 {
		return fmt.Errorf("workspace %s: hold timeout must not be negative", id)
	}
	if s.MaxLifetime != nil && *s.MaxLifetime < 0 {
		return fmt.Errorf("workspace %s: maximum lifetime must not be negative", id)
	}
	p.workspaces[id] = s
	return nil
}

// RemoveWorkspace stops claiming from a workspace. Its tasks run on; its
// bindings in hold, which nothing can renew any more, are reset, and so is
// each of its other bindings when its last task ends.
func (p *Policy) RemoveWorkspace(id string) []Decision {
	delete(p.workspaces, id)
	delete(p.backoff, id)
	delete(p.pending, id)
	var ds []Decision
	for _, b := range p.bindings(func(b *binding) bool { return b.workspace == id && b.state == bindingHold }) {
		ds = append(ds, p.resetBinding(b, Reset{Pod: b.pod.id, Reason: reasonRemoved}))
	}
	return ds
}

// PodAcquired records a pod the manager acquired from the sandbox service,
// clean and lendable, with the workspaces its daemon registered for. Tasks
// waiting for a pod are placed on it first.
func (p *Policy) PodAcquired(pod string, workspaces ...string) []Decision {
	pd := p.pods[pod]
	if pd == nil {
		pd = &podState{id: pod, workspaces: map[string]bool{}}
		p.pods[pod] = pd
	}
	for _, ws := range workspaces {
		pd.workspaces[ws] = true
	}
	return p.placeWaiting(false)
}

// PodRegistered records that the daemon of a held pod registered for a
// workspace, so the pod may be lent to it. A pod the manager does not hold is
// ignored: the workspaces come with PodAcquired.
func (p *Policy) PodRegistered(pod, workspace string) []Decision {
	pd := p.pods[pod]
	if pd == nil {
		return nil
	}
	pd.workspaces[workspace] = true
	return p.placeWaiting(false)
}

// PodLost records that a held pod went silent: the sandbox service no longer
// lends it. Its started tasks are failed with failReason and their leases
// ended with endReason; tasks placed on it that it had not picked up wait for
// another pod. The pod is forgotten.
func (p *Policy) PodLost(pod, failReason, endReason string) []Decision {
	return p.podGone(pod, failReason, endReason)
}

// PodInterrupted records that a held pod's daemon or container restarted by
// itself, so its started tasks are gone. It is handled as PodLost: the
// sandbox service resets the pod itself, so it is forgotten without a Reset.
func (p *Policy) PodInterrupted(pod, failReason, endReason string) []Decision {
	return p.podGone(pod, failReason, endReason)
}

func (p *Policy) podGone(pod, failReason, endReason string) []Decision {
	pd := p.pods[pod]
	if pd == nil {
		return nil
	}
	ds := p.vacate(pd, failReason, endReason)
	delete(p.pods, pod)
	return append(ds, p.placeWaiting(false)...)
}

// TaskStarted records that the pod picked up a task placed on it.
func (p *Policy) TaskStarted(id string) {
	if ts := p.tasks[id]; ts != nil {
		ts.started = true
	}
}

// TaskEnded records that a task is over. Its lease ends; a binding left with
// no task enters hold, or is reset at once when its hold timeout is zero or
// its key is unique to the task that ended (task rule, no initiator).
// Nothing is cleared between the leases of one binding.
func (p *Policy) TaskEnded(id, reason string) []Decision {
	ds := []Decision{EndLease{Task: id, Reason: reason}}
	ts := p.tasks[id]
	if ts == nil {
		return ds
	}
	p.forget(ts)
	if b := ts.binding; b != nil && len(b.tasks) == 0 {
		now := p.cfg.Now()
		_, served := p.workspaces[b.workspace]
		switch hold := p.holdTimeout(b.workspace); {
		case !served:
			ds = append(ds, p.resetBinding(b, Reset{Pod: b.pod.id, Reason: reasonRemoved, Task: id}))
		case p.pastLifetime(b, now):
			ds = append(ds, p.resetBinding(b, Reset{Pod: b.pod.id, Reason: reasonLifetime, Task: id}))
		case hold > 0 && !uniqueKey(b.key):
			b.state = bindingHold
			b.heldSince = now
			b.holdUntil = now.Add(hold)
		default:
			ds = append(ds, p.resetBinding(b, Reset{Pod: b.pod.id, Reason: "task ended: " + reason, Task: id}))
		}
	}
	return append(ds, p.placeWaiting(false)...)
}

// Busy reports whether a workspace still has tasks or claims in flight here.
func (p *Policy) Busy(workspace string) bool {
	for _, ts := range p.tasks {
		if ts.Workspace == workspace {
			return true
		}
	}
	for _, r := range p.reservations {
		if r.workspace == workspace {
			return true
		}
	}
	return false
}

// forget drops a task from the policy.
func (p *Policy) forget(ts *taskState) {
	delete(p.tasks, ts.ID)
	if b := ts.binding; b != nil {
		delete(b.tasks, ts.ID)
		b.used = b.used.sub(TaskDemand)
	}
	p.waiting = slices.DeleteFunc(p.waiting, func(w *taskState) bool { return w == ts })
}

// vacate takes everything off a pod: its reservation is dropped, its started
// tasks are failed and their leases ended, and the tasks it had not picked up
// wait for another pod.
func (p *Policy) vacate(pd *podState, failReason, endReason string) []Decision {
	if r := pd.reservation; r != nil {
		delete(p.reservations, r.id)
		pd.reservation = nil
	}
	now := p.cfg.Now()
	b := pd.binding
	if b == nil {
		return nil
	}
	pd.binding = nil
	ids := make([]string, 0, len(b.tasks))
	for id := range b.tasks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var ds []Decision
	for _, id := range ids {
		ts := b.tasks[id]
		ts.binding = nil
		if ts.started {
			delete(p.tasks, id)
			ds = append(ds, Fail{Task: id, Workspace: ts.Workspace, Reason: failReason}, EndLease{Task: id, Reason: endReason})
			continue
		}
		ts.waitingSince = now
		p.waiting = append(p.waiting, ts)
	}
	return ds
}
