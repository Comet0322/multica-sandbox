package lending

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// holdTimeout is the hold timeout for bindings opened by a tenant's tasks.
func (p *Policy) holdTimeout(tenant string) time.Duration {
	if s, ok := p.tenants[tenant]; ok && s.HoldTimeout != nil {
		return *s.HoldTimeout
	}
	return p.cfg.HoldTimeout
}

// resetBinding ends a binding: its pod is handed back and forgotten.
func (p *Policy) resetBinding(b *binding, d Reset) Decision {
	b.pod.binding = nil
	return p.handBack(b.pod, d)
}

// handBack forgets a pod the manager hands back to be reset.
func (p *Policy) handBack(pd *podState, d Reset) Decision {
	delete(p.pods, pd.id)
	return d
}

// Reasons for the resets and failures the maximum lifetime causes.
const (
	// DefaultRemovedReason is the reset reason of a removed tenant's pods
	// when Config.RemovedReason is empty.
	DefaultRemovedReason = "tenant removed"
	reasonLifetime       = "maximum lifetime reached"
	reasonGracePassed    = "maximum lifetime and drain grace passed"
	reasonGraceFailTask  = "the sandbox pod reached its maximum lifetime and the task ran past the drain grace period"
)

// maxLifetime is the maximum lifetime for bindings a tenant's tasks open.
func (p *Policy) maxLifetime(tenant string) time.Duration {
	if s, ok := p.tenants[tenant]; ok && s.MaxLifetime != nil {
		return *s.MaxLifetime
	}
	return p.cfg.MaxLifetime
}

// pastLifetime reports whether a binding is past its maximum lifetime, so it
// takes no new task.
func (p *Policy) pastLifetime(b *binding, now time.Time) bool {
	return b.lifetime > 0 && !uniqueKey(b.key) && !now.Before(b.openedAt.Add(b.lifetime))
}

// Tick lets time pass: displaced tasks that found no pod within the placement
// wait are failed, pods whose hold ran out or whose binding reached its
// maximum lifetime in hold are reset, and the tasks of a binding past its
// maximum lifetime and drain grace are failed and its pod reset.
func (p *Policy) Tick() []Decision {
	ds := p.placeWaiting(false)
	now := p.cfg.Now()
	ds = append(ds, p.expireHolds(now)...)
	ds = append(ds, p.expireLifetimes(now)...)
	still := p.waiting[:0]
	for _, ts := range p.waiting {
		if now.Sub(ts.waitingSince) > p.cfg.PlacementWait {
			delete(p.tasks, ts.ID)
			ds = append(ds, Fail{Task: ts.ID, Tenant: ts.Tenant, Reason: "no sandbox pod could take the task in time"})
			continue
		}
		still = append(still, ts)
	}
	p.waiting = still
	return append(ds, p.placeWaiting(false)...)
}

// bindings lists the bindings that match, in pod order. A binding whose pod
// is reserved for a claim in flight is left to the claim's outcome.
func (p *Policy) bindings(match func(b *binding) bool) []*binding {
	var out []*binding
	for _, pd := range p.pods {
		if b := pd.binding; b != nil && pd.reservation == nil && match(b) {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b *binding) int { return strings.Compare(a.pod.id, b.pod.id) })
	return out
}

// expireHolds resets the pods whose hold ran out by now, or whose binding in
// hold reached its maximum lifetime.
func (p *Policy) expireHolds(now time.Time) []Decision {
	var ds []Decision
	for _, b := range p.bindings(func(b *binding) bool { return b.state == bindingHold }) {
		switch {
		case p.pastLifetime(b, now):
			ds = append(ds, p.resetBinding(b, Reset{Pod: b.pod.id, Reason: reasonLifetime}))
		case !keptKey(b.key) && !now.Before(b.holdUntil):
			ds = append(ds, p.resetBinding(b, Reset{Pod: b.pod.id, Reason: "hold expired"}))
		}
	}
	return ds
}

// expireLifetimes fails the started tasks of the bindings past their maximum
// lifetime and drain grace, and resets their pods. Tasks placed there but not
// picked up wait for another pod.
func (p *Policy) expireLifetimes(now time.Time) []Decision {
	var ds []Decision
	over := p.bindings(func(b *binding) bool {
		return b.state == bindingRunning && p.pastLifetime(b, now.Add(-p.cfg.DrainGrace))
	})
	for _, b := range over {
		pd := b.pod
		ds = append(ds, p.vacate(pd, reasonGraceFailTask, reasonGracePassed)...)
		ds = append(ds, p.handBack(pd, Reset{Pod: pd.id, Reason: reasonGracePassed}))
	}
	return ds
}

// evictable is the held pod serving a tenant whose hold a task of the tenant
// may end (see mayEvict), not reserved; nil when there is none. The hold of
// the tenant furthest over its share goes first, then the one in hold
// longest.
func (p *Policy) evictable(ws string) *podState {
	g := p.guarantees()
	var best *podState
	for _, pd := range p.pods {
		b := pd.binding
		if !pd.tenants[ws] || pd.reservation != nil || b == nil || b.state != bindingHold || !p.mayEvict(b.tenant, ws) {
			continue
		}
		if best == nil {
			best = pd
			continue
		}
		bb := best.binding
		if c := compareShares(p.shareOf(b.tenant, g, 0), p.shareOf(bb.tenant, g, 0)); c != 0 {
			if c > 0 {
				best = pd
			}
			continue
		}
		if b.heldSince.Before(bb.heldSince) || (b.heldSince.Equal(bb.heldSince) && pd.id < best.id) {
			best = pd
		}
	}
	return best
}

// evict ends the hold of a pod for a task of another affinity key: the pod
// is handed back, and the task, which waits for a pod, takes the next one
// acquired.
func (p *Policy) evict(pd *podState, ts *taskState) Decision {
	ts.evictedFor = true
	return p.resetBinding(pd.binding, Reset{Pod: pd.id, Reason: fmt.Sprintf("evicted for task %s of another affinity key", ts.ID)})
}
