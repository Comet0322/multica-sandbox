package lending

import (
	"cmp"
	"slices"
)

// Fair sharing between tenants. A pod serves one tenant at a time (a
// binding never mixes tenants), so tenants share the pool in whole pods: a
// tenant occupies the pods its bindings hold, running or in hold, and the
// pods reserved for its claims that have no binding yet.
//
// Each tenant has a guarantee and a weight (TenantSettings):
//
//   - The guarantee is reserved for it. While the tenant occupies fewer pods
//     than its guarantee, the rest is kept from other tenants, even when the
//     tenant has nothing to run. The guarantees are scaled down in
//     proportion when they add up to more than the live pods.
//   - The weight shares out the pods beyond the guarantees: the next claim
//     goes to the tenant with the least occupancy beyond its guarantee per
//     unit of weight. Priority and age order tasks within one tenant only.
//
// Running tasks are never preempted; a tenant over its share gives pods back
// as its bindings end, and its holds are the first evicted.

// weightOf is a tenant's weight; zero counts as one.
func weightOf(s TenantSettings) int {
	return max(s.Weight, 1)
}

// occupied pods counts the pods a tenant occupies: those its bindings hold
// and those reserved for its claims that have no binding.
func (p *Policy) occupiedPods(tenant string) int {
	n := 0
	for _, pd := range p.pods {
		switch {
		case pd.binding != nil:
			if pd.binding.tenant == tenant {
				n++
			}
		case pd.reservation != nil:
			if pd.reservation.tenant == tenant {
				n++
			}
		}
	}
	return n
}

// guarantees are the guarantees in effect for the live pods: as configured
// when they fit, else each scaled down in proportion (rounded down).
func (p *Policy) guarantees() map[string]int {
	out := map[string]int{}
	sum := 0
	for id, s := range p.tenants {
		out[id] = s.Guarantee
		sum += s.Guarantee
	}
	if sum <= p.livePods {
		return out
	}
	for id, g := range out {
		out[id] = g * p.livePods / sum
	}
	return out
}

// share is a tenant's standing for fair sharing: its occupancy beyond its
// guarantee, and its weight.
type share struct {
	excess, weight int
}

func (p *Policy) shareOf(tenant string, guarantees map[string]int, extra int) share {
	return share{excess: p.occupiedPods(tenant) + extra - guarantees[tenant], weight: weightOf(p.tenants[tenant])}
}

// compareShares orders a before b when a has less excess per unit of weight.
func compareShares(a, b share) int {
	return cmp.Compare(a.excess*b.weight, b.excess*a.weight)
}

// mayTakePod reports whether a tenant may take one more pod that is not
// occupied now: it is under its guarantee, or the pods nobody occupies still
// cover what the other tenants' guarantees keep for them afterwards.
func (p *Policy) mayTakePod(tenant string) bool {
	g := p.guarantees()
	if p.occupiedPods(tenant) < g[tenant] {
		return true
	}
	kept, occupied := 0, 0
	for id := range p.tenants {
		if id != tenant {
			kept += max(g[id]-p.occupiedPods(id), 0)
		}
	}
	if kept == 0 {
		return true
	}
	for _, pd := range p.pods {
		if pd.binding != nil || pd.reservation != nil {
			occupied++
		}
	}
	return p.livePods-occupied-1 >= kept
}

// fairOrder orders the tenants with a queued task: the least excess per unit
// of weight first, then the one whose next task has waited longest.
func (p *Policy) fairOrder(heads map[string]Pending) []string {
	g := p.guarantees()
	order := make([]string, 0, len(heads))
	for ws := range heads {
		order = append(order, ws)
	}
	slices.SortFunc(order, func(a, b string) int {
		if c := compareShares(p.shareOf(a, g, 0), p.shareOf(b, g, 0)); c != 0 {
			return c
		}
		if c := heads[a].CreatedAt.Compare(heads[b].CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return order
}

// mayEvict reports whether a hold of tenant victim may be ended for a task of
// tenant taker. A hold runs nothing, so it never stands in another tenant's
// way beyond its own tenant's guarantee: a tenant may end its own holds, and
// another tenant's while that tenant occupies more pods than its guarantee.
// Which hold goes first is evictable's choice.
func (p *Policy) mayEvict(victim, taker string) bool {
	return victim == taker || p.occupiedPods(victim) > p.guarantees()[victim]
}

// ShareInfo is a tenant's fair share, for the inventory.
type ShareInfo struct {
	Tenant string
	// Guarantee and Weight are as configured; GuaranteeInEffect is the
	// guarantee scaled to the live pods.
	Guarantee, GuaranteeInEffect, Weight int
	// Occupied are the pods the tenant occupies; Holding those of them in
	// hold.
	Occupied, Holding int
	// Target is the number of pods the tenant would hold if every tenant
	// with work were given its share of the live pods (see Shares).
	Target int
	// Queued counts the tasks on its pending list not claimed yet.
	Queued int
}

// Shares reports every served tenant's fair share. The target splits the
// live pods as the policy aims to: each tenant first gets its guarantee in
// effect, and the rest goes by weight, one pod at a time, to the tenants that
// have work (a task queued or occupying a pod), never past what that work
// can use: its occupied pods plus its queued tasks.
func (p *Policy) Shares() []ShareInfo {
	g := p.guarantees()
	out := make([]ShareInfo, 0, len(p.tenants))
	target := map[string]int{}
	demand := map[string]int{}
	left := p.livePods
	for id, s := range p.tenants {
		info := ShareInfo{Tenant: id, Guarantee: s.Guarantee, GuaranteeInEffect: g[id], Weight: weightOf(s), Occupied: p.occupiedPods(id)}
		for _, pd := range p.pods {
			if b := pd.binding; b != nil && b.tenant == id && b.state == bindingHold {
				info.Holding++
			}
		}
		if l := p.pending[id]; l != nil {
			info.Queued = len(l.queued)
		}
		demand[id] = info.Occupied + info.Queued
		target[id] = g[id]
		left -= g[id]
		out = append(out, info)
	}
	for left > 0 {
		best := ""
		for id := range p.tenants {
			if target[id] >= demand[id] {
				continue
			}
			if best == "" {
				best = id
				continue
			}
			a := share{excess: target[id] - g[id], weight: weightOf(p.tenants[id])}
			b := share{excess: target[best] - g[best], weight: weightOf(p.tenants[best])}
			if c := compareShares(a, b); c < 0 || (c == 0 && id < best) {
				best = id
			}
		}
		if best == "" {
			break
		}
		target[best]++
		left--
	}
	for i := range out {
		out[i].Target = target[out[i].Tenant]
	}
	slices.SortFunc(out, func(a, b ShareInfo) int { return cmp.Compare(a.Tenant, b.Tenant) })
	return out
}

// LivePods is the number of live pods the policy last heard of (see
// ClaimRequest.LivePods).
func (p *Policy) LivePods() int { return p.livePods }
