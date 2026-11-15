package policy

import (
	"cmp"
	"slices"
)

// Fair sharing between tenants. A pod never serves another tenant, so
// tenants share the pods in whole pods: a tenant occupies its running, held
// and idle pods, and the warm pods its claims in flight were made against.
//
// Each tenant has a guarantee and a weight (TenantSettings):
//
//   - The guarantee is kept for it. While the tenant occupies fewer pods
//     than its guarantee, the rest is kept from other tenants, even when the
//     tenant has nothing to run. The guarantees are scaled down in
//     proportion when they add up to more than the budget.
//   - The weight shares out the pods beyond the guarantees: the next claim
//     goes to the tenant with the least occupancy beyond its guarantee per
//     unit of weight. Priority and age order tasks within one tenant only.
//
// Running tasks are never preempted. When the budget is full, the held and
// idle pods of the tenant furthest over its share go first (see victim), and
// never a pod within a tenant's guarantee for another tenant.

// weightOf is a tenant's weight; zero counts as one.
func weightOf(s TenantSettings) int { return max(s.Weight, 1) }

// occupiedBy reports whether a pod counts toward a tenant's occupancy.
func occupiedBy(pd *podState, tenant string) bool {
	if pd.tenant != "" {
		return pd.tenant == tenant
	}
	return pd.claim != nil && pd.claim.target.Tenant == tenant
}

// occupied reports whether a pod counts toward some tenant's occupancy.
func occupied(pd *podState) bool { return pd.tenant != "" || pd.claim != nil }

// occupiedPods counts the pods a tenant occupies.
func (p *Policy) occupiedPods(tenant string) int {
	n := 0
	for _, pd := range p.pods {
		if occupiedBy(pd, tenant) {
			n++
		}
	}
	return n
}

// budget is the number of pods the guarantees are scaled to: the budget,
// or the live pods when the pool keeps the budget.
func (p *Policy) budget() int {
	if p.cfg.MaxPods > 0 {
		return p.cfg.MaxPods
	}
	return p.livePods
}

// guarantees are the guarantees in effect: as configured when they fit,
// else each scaled down in proportion (rounded down).
func (p *Policy) guarantees() map[string]int {
	out := map[string]int{}
	sum := 0
	for id, s := range p.tenants {
		out[id] = s.Guarantee
		sum += s.Guarantee
	}
	budget := p.budget()
	if sum <= budget {
		return out
	}
	for id, g := range out {
		out[id] = g * budget / sum
	}
	return out
}

// share is a tenant's standing for fair sharing: its occupancy beyond its
// guarantee, and its weight.
type share struct {
	excess, weight int
}

func (p *Policy) shareOf(tenant string, guarantees map[string]int) share {
	return share{excess: p.occupiedPods(tenant) - guarantees[tenant], weight: weightOf(p.tenants[tenant])}
}

// compareShares orders a before b when a has less excess per unit of weight.
func compareShares(a, b share) int {
	return cmp.Compare(a.excess*b.weight, b.excess*a.weight)
}

// mayTakePod reports whether a tenant may take one more pod that nobody
// occupies: it is under its guarantee, or the pods nobody occupies still
// cover what the other tenants' guarantees keep for them afterwards.
func (p *Policy) mayTakePod(tenant string) bool {
	g := p.guarantees()
	if p.occupiedPods(tenant) < g[tenant] {
		return true
	}
	kept, taken := 0, 0
	for id := range p.tenants {
		if id != tenant {
			kept += max(g[id]-p.occupiedPods(id), 0)
		}
	}
	if kept == 0 {
		return true
	}
	for _, pd := range p.pods {
		if occupied(pd) {
			taken++
		}
	}
	return p.budget()-taken-1 >= kept
}

// fairOrder orders the targets with a queued task: the tenant with the least
// excess per unit of weight first, then the one whose next task has waited
// longest.
func (p *Policy) fairOrder(heads map[Target]Pending) []Target {
	g := p.guarantees()
	order := make([]Target, 0, len(heads))
	for tg := range heads {
		order = append(order, tg)
	}
	slices.SortFunc(order, func(a, b Target) int {
		if a.Tenant != b.Tenant {
			if c := compareShares(p.shareOf(a.Tenant, g), p.shareOf(b.Tenant, g)); c != 0 {
				return c
			}
		}
		if c := heads[a].CreatedAt.Compare(heads[b].CreatedAt); c != 0 {
			return c
		}
		return cmp.Or(cmp.Compare(a.Tenant, b.Tenant), cmp.Compare(a.Kind, b.Kind))
	})
	return order
}

// mayDelete reports whether a held or idle pod of tenant victim may be
// deleted for a task of tenant taker: a tenant may give up its own, and
// another's while that tenant occupies more pods than its guarantee.
func (p *Policy) mayDelete(victim, taker string, g map[string]int) bool {
	return victim == taker || p.occupiedPods(victim) > g[victim]
}

// victim is the pod to delete when the budget is full and a task of taker
// needs a pod; nil when none may go. Idle pool runtime pods go first, then held pool
// pods, then held clean pods; within each, the pod of the tenant furthest
// over its share, then the one held or idle longest. A pod a claim in flight
// was made against is left alone. take says the taker may take a pod
// nobody occupies; when it may not, only its own pods may go.
func (p *Policy) victim(taker string, take bool) *podState {
	g := p.guarantees()
	rank := func(pd *podState) int {
		switch {
		case pd.phase == PhaseIdle:
			return 1
		case pd.phase == PhaseHeld && pd.kind == KindPool:
			return 2
		case pd.phase == PhaseHeld:
			return 3
		}
		return 0
	}
	var best *podState
	for _, pd := range p.sortedPods() {
		if rank(pd) == 0 || pd.claim != nil || !p.mayDelete(pd.tenant, taker, g) || (!take && pd.tenant != taker) {
			continue
		}
		if best == nil {
			best = pd
			continue
		}
		if c := cmp.Compare(rank(pd), rank(best)); c != 0 {
			if c < 0 {
				best = pd
			}
			continue
		}
		if pd.tenant != best.tenant {
			if c := compareShares(p.shareOf(pd.tenant, g), p.shareOf(best.tenant, g)); c != 0 {
				if c > 0 {
					best = pd
				}
				continue
			}
		}
		if pd.since.Before(best.since) {
			best = pd
		}
	}
	return best
}

// ShareInfo is a tenant's fair share, for the inventory.
type ShareInfo struct {
	Tenant string
	// Guarantee and Weight are as configured; GuaranteeInEffect is the
	// guarantee scaled to the budget.
	Guarantee, GuaranteeInEffect, Weight int
	// Occupied are the pods the tenant occupies; Holding and Idle those of
	// them held and idle.
	Occupied, Holding, Idle int
	// Target is the number of pods the tenant would occupy if every tenant
	// with work were given its share (see Shares).
	Target int
	// Queued counts the tasks on its pending lists not claimed yet.
	Queued int
}

// Shares reports every served tenant's fair share. The target splits the
// budget as the policy aims to: each tenant first gets its guarantee in
// effect, and the rest goes by weight, one pod at a time, to the tenants that
// have work (a task queued or a pod occupied), never past what that work can
// use: its occupied pods plus its queued tasks.
func (p *Policy) Shares() []ShareInfo {
	g := p.guarantees()
	out := make([]ShareInfo, 0, len(p.tenants))
	target := map[string]int{}
	demand := map[string]int{}
	left := p.budget()
	for id, s := range p.tenants {
		info := ShareInfo{Tenant: id, Guarantee: s.Guarantee, GuaranteeInEffect: g[id], Weight: weightOf(s), Occupied: p.occupiedPods(id)}
		for _, pd := range p.pods {
			if pd.tenant == id && pd.phase == PhaseHeld {
				info.Holding++
			}
			if pd.tenant == id && pd.phase == PhaseIdle {
				info.Idle++
			}
		}
		info.Queued = len(p.Queued(id))
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
