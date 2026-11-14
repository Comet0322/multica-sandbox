package lending

import (
	"cmp"
	"slices"
	"time"
)

// StatusQueued is the status of a pending task nobody has claimed yet.
const StatusQueued = "queued"

// Pending is a task on a workspace's pending list, as the server lists it.
type Pending struct {
	ID        string
	Priority  int
	CreatedAt time.Time
	// Status is the server's task status. The list also holds tasks already
	// handed to a claimer ("dispatched"); only StatusQueued ones are ordered.
	Status string
}

// pendingList is the last pending list read for a workspace.
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

// PendingChanged records a workspace's pending list, read on a dispatch tick.
// It replaces the previous one.
func (p *Policy) PendingChanged(workspace string, tasks []Pending) {
	l := &pendingList{}
	for _, t := range tasks {
		if t.Status == StatusQueued {
			l.queued = append(l.queued, t)
		}
	}
	p.pending[workspace] = l
}

// PendingUnknown forgets a workspace's pending list, when it could not be read.
// The workspace is then claimed from in turn whenever a pod asks, as if the
// manager read no lists at all.
func (p *Policy) PendingUnknown(workspace string) {
	delete(p.pending, workspace)
}

// queueEntry is one queued task of the global queue.
type queueEntry struct {
	Pending
	workspace string
}

// claimOrder lists the workspaces to try for a claim, best first. The global
// queue comes first: the queued tasks of every workspace in the request,
// ordered by priority descending and then creation time ascending, as the
// server orders them; tasks that claims in flight will take are left out.
// Then come, in turn after the workspace claimed from last, the workspaces
// still worth one claim without a queued task: those whose pending list is
// unknown, and those not yet probed since their last list.
func (p *Policy) claimOrder(req ClaimRequest) []string {
	inFlight := map[string]int{}
	for _, r := range p.reservations {
		inFlight[r.workspace]++
	}
	var queue []queueEntry
	for _, ws := range req.Workspaces {
		l := p.pending[ws]
		if l == nil {
			continue
		}
		for _, t := range l.queued[min(inFlight[ws], len(l.queued)):] {
			queue = append(queue, queueEntry{Pending: t, workspace: ws})
		}
	}
	slices.SortStableFunc(queue, func(a, b queueEntry) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var order []string
	for _, e := range queue {
		if !slices.Contains(order, e.workspace) {
			order = append(order, e.workspace)
		}
	}
	n := len(req.Workspaces)
	start := 0
	if i := slices.Index(req.Workspaces, p.lastWorkspace); i >= 0 {
		start = i + 1
	}
	for i := 0; i < n; i++ {
		ws := req.Workspaces[(start+i)%n]
		if l := p.pending[ws]; (l == nil || !l.probed) && !slices.Contains(order, ws) {
			order = append(order, ws)
		}
	}
	return order
}

// claimable reports whether a claim may be sent to a workspace now: it is
// served, not excluded from this round, not backing off, and one more task
// fits in its quota.
func (p *Policy) claimable(ws string, req ClaimRequest) bool {
	s, served := p.workspaces[ws]
	if !served || slices.Contains(req.Exclude, ws) || p.cfg.Now().Before(p.backoff[ws]) {
		return false
	}
	return s.Quota == 0 || p.occupied(ws).add(TaskDemand).Slots <= s.Quota
}

// claimSent records a claim sent to a workspace: when no queued task of its
// pending list accounts for it, it is the one claim allowed per list.
func (p *Policy) claimSent(ws string) {
	l := p.pending[ws]
	if l == nil {
		return
	}
	n := 0
	for _, r := range p.reservations {
		if r.workspace == ws {
			n++
		}
	}
	if n > len(l.queued) {
		l.probed = true
	}
}

// claimAnswered updates a workspace's pending list with a claim's outcome: a
// returned task is no longer queued, and an empty claim means nothing more is
// claimable there until the next list.
func (p *Policy) claimAnswered(ws string, t *Task, failed bool) {
	l := p.pending[ws]
	if l == nil || failed {
		return
	}
	if t == nil {
		l.queued, l.probed = nil, true
		return
	}
	l.queued = slices.DeleteFunc(l.queued, func(q Pending) bool { return q.ID == t.ID })
}

// ClaimRequest asks for a claim on behalf of pods, in order of preference.
type ClaimRequest struct {
	// Pods are the pods a reservation may be made on, the preferred first.
	Pods []string
	// Workspaces are the workspaces that may be claimed from (the manager has
	// runtimes for them), in rotation order for claims no queued task orders.
	Workspaces []string
	// Exclude are workspaces already tried in this round.
	Exclude []string
	// NoCapacity says the sandbox service has no free pod to acquire this
	// round. Only then is a pod in hold reserved or its hold ended for a
	// waiting task, so a hold never takes the place of a free pod.
	NoCapacity bool
}

// RequestClaim reserves a place that can certainly take a task and picks the
// workspace to claim from: the first workspace in claim order (see
// claimOrder) that may be claimed from now and that one of the request's pods,
// the preferred first, can take a new binding for. It returns at most one
// Claim. Nothing is claimed while displaced tasks still wait for a pod, nor
// when no held pod can take a task: the task then stays queued upstream, and,
// unless the request says there is no capacity, an Acquire asks for a pod.
func (p *Policy) RequestClaim(req ClaimRequest) []Decision {
	ds := p.placeWaiting(req.NoCapacity)
	if len(p.waiting) > 0 {
		if !req.NoCapacity {
			ds = append(ds, Acquire{})
		}
		return ds
	}
	for _, ws := range p.claimOrder(req) {
		if !p.claimable(ws, req) {
			continue
		}
		pd, wantPod := p.reservePod(ws, req.Pods, req.NoCapacity)
		if pd == nil {
			if wantPod && !req.NoCapacity {
				return append(ds, Acquire{})
			}
			continue
		}
		p.nextRes++
		r := &reservation{id: p.nextRes, pod: pd, workspace: ws}
		p.reservations[r.id] = r
		pd.reservation = r
		p.lastWorkspace = ws
		p.claimSent(ws)
		return append(ds, Claim{Workspace: ws, Reservation: r.id, Pod: pd.id})
	}
	return ds
}

// reservePod is the pod among pods to reserve for a claim from a workspace:
// the first idle one; else, when no held pod serving the workspace is idle
// and evict is set, the one whose binding has been in hold longest, provided
// it is among pods. A binding with a running task is never reserved. wantPod
// reports that no held pod serving the workspace is idle, so one acquired
// could take the claim.
func (p *Policy) reservePod(ws string, pods []string, evict bool) (pd *podState, wantPod bool) {
	for _, id := range pods {
		if pd := p.pods[id]; pd != nil && pd.workspaces[ws] && p.reservable(pd) {
			return pd, false
		}
	}
	if p.freePod(ws) != nil {
		return nil, false
	}
	if pd := p.evictable(ws); evict && pd != nil && slices.Contains(pods, pd.id) {
		return pd, false
	}
	return nil, true
}

// reservable reports whether the pod can certainly take a new binding.
func (p *Policy) reservable(pd *podState) bool {
	return pd.binding == nil && pd.reservation == nil
}

// occupied is the capacity a workspace's quota counts: its running tasks
// (placed, started or waiting for a pod) and its claims in flight. Room left
// in its bindings does not count.
func (p *Policy) occupied(workspace string) Resources {
	var used Resources
	for _, ts := range p.tasks {
		if ts.Workspace == workspace {
			used = used.add(TaskDemand)
		}
	}
	for _, r := range p.reservations {
		if r.workspace == workspace {
			used = used.add(TaskDemand)
		}
	}
	return used
}

// ClaimReturned records a claim's outcome: the task it returned, nil when it
// returned none, with failed set when the upstream call failed.
//
// A task goes to the most recently used binding of its affinity key that has
// room, releasing the reservation, or opens a new binding on the reserved pod.
// When the reserved pod is in hold for another key, its hold is evicted: the
// pod is handed back, and the task waits for the next pod acquired, which
// takes it before any new claim is made.
func (p *Policy) ClaimReturned(id ReservationID, t *Task, failed bool) []Decision {
	r := p.reservations[id]
	if r == nil {
		// The reserved pod went away while the claim was in flight.
		if t == nil {
			return nil
		}
		return p.placeOrphan(t)
	}
	delete(p.reservations, id)
	r.pod.reservation = nil
	if failed {
		p.backoff[r.workspace] = p.cfg.Now().Add(p.cfg.Backoff)
	} else {
		delete(p.backoff, r.workspace)
	}
	p.claimAnswered(r.workspace, t, failed)
	release := Release{Reservation: id, Pod: r.pod.id}
	if t == nil {
		return []Decision{release}
	}
	ts := p.newTask(*t)
	if b := p.bindingFor(ts); b != nil {
		return []Decision{p.join(b, ts), release}
	}
	if r.pod.binding != nil {
		p.wait(ts)
		return []Decision{p.evict(r.pod, ts)}
	}
	return []Decision{p.open(r.pod, ts)}
}
