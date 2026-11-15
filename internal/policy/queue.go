package policy

import (
	"cmp"
	"slices"
	"time"
)

// StatusQueued is the status of a pending task nobody has claimed yet.
const StatusQueued = "queued"

// Pending is a task on a pending list, as the server lists it.
type Pending struct {
	ID        string
	Priority  int
	CreatedAt time.Time
	// Status is the server's task status. The list also holds tasks already
	// handed to a claimer ("dispatched"); only StatusQueued ones are ordered.
	Status string
}

// pendingList is the last pending list read for a target.
type pendingList struct {
	// queued are the list's queued tasks not yet claimed here.
	queued []Pending
	// probed is set once a claim was sent that no queued task accounted for,
	// or a claim returned nothing. No such claim is sent again until the next
	// list: the claim lets the server promote deferred retries and reclaim
	// stale dispatches, which never show as queued, so it is worth one try per
	// list and no more.
	probed bool
}

// PendingChanged records the pending list of a tenant's runtimes of one
// kind, read on a dispatch tick. It replaces the previous one.
func (p *Policy) PendingChanged(tenant string, kind Kind, tasks []Pending) {
	l := &pendingList{}
	for _, t := range tasks {
		if t.Status == StatusQueued {
			l.queued = append(l.queued, t)
		}
	}
	p.pending[Target{tenant, kind}] = l
}

// PendingUnknown forgets a pending list that could not be read. The target
// is then claimed for in turn, as if no lists were read at all.
func (p *Policy) PendingUnknown(tenant string, kind Kind) {
	delete(p.pending, Target{tenant, kind})
}

// claimOrder lists the targets to try for a claim, best first. The targets
// with a queued task come first, in fair order (see fairOrder); tasks that
// claims in flight will take are left out. Then come, in turn after the
// target claimed for last, the targets still worth one claim without a
// queued task: those whose pending list is unknown, and those not yet probed
// since their last list.
func (p *Policy) claimOrder(req ClaimRequest) []Target {
	inFlight := map[Target]int{}
	for _, r := range p.reservations {
		inFlight[r.target]++
	}
	var targets []Target
	for _, t := range req.Tenants {
		for _, k := range req.Kinds {
			targets = append(targets, Target{t, k})
		}
	}
	heads := map[Target]Pending{}
	for _, tg := range targets {
		l := p.pending[tg]
		if l == nil {
			continue
		}
		if rest := l.queued[min(inFlight[tg], len(l.queued)):]; len(rest) > 0 {
			heads[tg] = head(rest)
		}
	}
	order := p.fairOrder(heads)
	n := len(targets)
	start := 0
	if i := slices.Index(targets, p.lastTarget); i >= 0 {
		start = i + 1
	}
	for i := 0; i < n; i++ {
		tg := targets[(start+i)%n]
		if l := p.pending[tg]; (l == nil || !l.probed) && !slices.Contains(order, tg) {
			order = append(order, tg)
		}
	}
	return order
}

func comparePending(a, b Pending) int {
	if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
		return c
	}
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// head is the task the server hands out first from a pending list: the
// highest priority, then the oldest.
func head(tasks []Pending) Pending { return slices.MinFunc(tasks, comparePending) }

// claimable reports whether a claim may be sent for a target now: its
// tenant is served, not excluded from this round, not backing off, and one
// more task fits in its quota.
func (p *Policy) claimable(tg Target, req ClaimRequest) bool {
	s, served := p.tenants[tg.Tenant]
	if !served || slices.Contains(req.Exclude, tg) || p.cfg.Now().Before(p.backoff[tg.Tenant]) {
		return false
	}
	return s.Quota == 0 || p.running(tg.Tenant)+1 <= s.Quota
}

// claimSent records a claim sent for a target: when no queued task of its
// pending list accounts for it, it is the one claim allowed per list.
func (p *Policy) claimSent(tg Target) {
	l := p.pending[tg]
	if l == nil {
		return
	}
	n := 0
	for _, r := range p.reservations {
		if r.target == tg {
			n++
		}
	}
	if n > len(l.queued) {
		l.probed = true
	}
}

// claimAnswered updates a target's pending list with a claim's outcome: a
// returned task is no longer queued, and an empty claim means nothing more is
// claimable there until the next list.
func (p *Policy) claimAnswered(tg Target, t *Task, failed bool) {
	l := p.pending[tg]
	if l == nil || failed {
		return
	}
	if t == nil {
		l.queued, l.probed = nil, true
		return
	}
	l.queued = slices.DeleteFunc(l.queued, func(q Pending) bool { return q.ID == t.ID })
}

// ClaimRequest asks for a claim, with what the caller knows of the pods the
// policy does not hold.
type ClaimRequest struct {
	// Tenants are the tenants that may be claimed from, in rotation order
	// for claims no queued task orders.
	Tenants []string
	// Kinds are the runtime kinds claimed for.
	Kinds []Kind
	// Exclude are targets already tried in this round.
	Exclude []Target
	// NoCapacity says the pool has no ready pod to give this round.
	NoCapacity bool
	// LivePods counts every pod that is not lost: warm, starting, running,
	// held and idle. With Config.MaxPods it tells whether the budget is
	// full; without it the guarantees are scaled to it.
	LivePods int
	// Starting counts the pods on their way to ready. While one is, nothing
	// is deleted to make room: it will do.
	Starting int
}

// RequestClaim picks the target to claim for and reserves a ready pod any
// task of it can run on: an idle pod of the tenant for the pool kind, else a
// warm pod. It returns at most one Claim. Nothing is claimed while tasks
// still wait for a pod. When the first target that may claim has no ready
// pod, the policy asks for one with Acquire, or, when the pool has none and
// the budget is full, deletes one with Delete (see victim); the task then
// stays queued upstream until the pod is ready.
func (p *Policy) RequestClaim(req ClaimRequest) []Decision {
	p.livePods = req.LivePods
	ds := p.placeWaiting()
	if len(p.waiting) > 0 {
		return append(ds, p.needPod(req, p.waiting[0].Tenant, true)...)
	}
	for _, tg := range p.claimOrder(req) {
		if !p.claimable(tg, req) {
			continue
		}
		take := p.mayTakePod(tg.Tenant)
		var pd *podState
		if tg.Kind == KindPool {
			pd = p.idlePod(tg.Tenant, nil)
		}
		if pd == nil && take {
			pd = p.warmPod(tg.Tenant, nil)
		}
		if pd == nil {
			// A probe of a list that shows nothing queued may ask for a
			// pod, but never deletes one: there may be no task to need it.
			if p.probing(tg) {
				if !req.NoCapacity && take {
					return append(ds, Acquire{})
				}
				continue
			}
			if need := p.needPod(req, tg.Tenant, take); need != nil {
				return append(ds, need...)
			}
			continue
		}
		p.nextRes++
		r := &reservation{id: p.nextRes, pod: pd, target: tg}
		p.reservations[r.id] = r
		pd.reservation = r
		p.lastTarget = tg
		p.claimSent(tg)
		return append(ds, Claim{Tenant: tg.Tenant, Kind: tg.Kind, Reservation: r.id, Pod: pd.id})
	}
	return ds
}

// probing reports whether a claim for the target would be a probe: its
// pending list was read and has no queued task left that claims in flight
// do not account for.
func (p *Policy) probing(tg Target) bool {
	l := p.pending[tg]
	if l == nil {
		return false
	}
	inFlight := 0
	for _, r := range p.reservations {
		if r.target == tg {
			inFlight++
		}
	}
	return len(l.queued) <= inFlight
}

// needPod is what gets a tenant a ready pod: Acquire while the pool may have
// one, else, when the budget is full and no pod is starting, the deletion of
// the pod the tenant may take the place of. Nil when nothing can be done
// now. take says the tenant may take a pod nobody occupies.
func (p *Policy) needPod(req ClaimRequest, tenant string, take bool) []Decision {
	if !req.NoCapacity {
		if take {
			return []Decision{Acquire{}}
		}
		return nil
	}
	if req.Starting > 0 || (p.cfg.MaxPods > 0 && req.LivePods < p.cfg.MaxPods) {
		return nil
	}
	pd := p.victim(tenant, take)
	if pd == nil {
		return nil
	}
	return []Decision{p.delete(pd, Delete{Pod: pd.id, Reason: reasonBudget})}
}

// ClaimReturned records a claim's outcome: the task it returned, nil when it
// returned none, with failed set when the upstream call failed. The task
// goes where placeFor says; the pod reserved for it is released and keeps
// its phase when the task goes elsewhere.
func (p *Policy) ClaimReturned(id ReservationID, t *Task, failed bool) []Decision {
	r := p.reservations[id]
	if r == nil {
		// The reserved pod went away while the claim was in flight.
		if t == nil {
			return nil
		}
		ts := &taskState{Task: *t}
		if pd := p.placeFor(ts, nil); pd != nil {
			return []Decision{p.put(pd, ts)}
		}
		p.wait(ts)
		return nil
	}
	delete(p.reservations, id)
	r.pod.reservation = nil
	if failed {
		p.backoff[r.target.Tenant] = p.cfg.Now().Add(p.cfg.Backoff)
	} else {
		delete(p.backoff, r.target.Tenant)
	}
	p.claimAnswered(r.target, t, failed)
	release := Release{Reservation: id, Pod: r.pod.id}
	if t == nil {
		return []Decision{release}
	}
	ts := &taskState{Task: *t}
	pd := p.placeFor(ts, r.pod)
	if pd == nil {
		p.wait(ts)
		return []Decision{release}
	}
	d := p.put(pd, ts)
	if pd == r.pod {
		return []Decision{d}
	}
	return []Decision{d, release}
}

// Queued lists a tenant's queued tasks not claimed yet, of every kind, in
// the order the server would hand them out: priority, then age.
func (p *Policy) Queued(tenant string) []Pending {
	var out []Pending
	for tg, l := range p.pending {
		if tg.Tenant == tenant {
			out = append(out, l.queued...)
		}
	}
	slices.SortFunc(out, comparePending)
	return out
}

// running counts what a tenant's quota counts: its tasks, placed, running
// or waiting for a pod, and its claims in flight.
func (p *Policy) running(tenant string) int {
	n := 0
	for _, ts := range p.tasks {
		if ts.Tenant == tenant {
			n++
		}
	}
	for _, r := range p.reservations {
		if r.target.Tenant == tenant {
			n++
		}
	}
	return n
}
