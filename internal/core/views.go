package core

import (
	"cmp"
	"slices"
	"time"

	"sandbox-manager/internal/policy"
)

// What the core reports for the inventory. The manager joins it with what the
// source and the pool report.

// PodView is what the core knows of one pod.
type PodView struct {
	// Held is set while the core holds the pod; Busy once it served anyone.
	Held, Busy bool
	// Tenant is the tenant of the pod's longest-held lease, empty with none.
	Tenant string
	// Initiators are the people whose tasks the pod holds, sorted.
	Initiators []string
	// Policy is what the policy knows of the pod: its phase, tenant, kind
	// and key. Nil when the core does not hold it.
	Policy *policy.PodInfo
}

// TenantView is what the core knows of one tenant.
type TenantView struct {
	ID string
	// Leased counts its active leases; Busy is set while it holds anything.
	Leased int
	Busy   bool
	// Draining is set for a removed tenant whose drain has not ended.
	Draining bool
}

// View is a snapshot of the core's state for the inventory.
type View struct {
	Pods    map[string]PodView
	Tenants []TenantView
	// Shares are the tenants' fair shares (see policy.Policy.Shares).
	Shares []policy.ShareInfo
	// LivePods is the number of live pods the shares were computed for.
	LivePods int
	// OldestLease is when the oldest active lease started, zero with none.
	OldestLease time.Time
	// Queued are each tenant's tasks waiting upstream, in claim order; a
	// tenant whose pending list is unknown has none.
	Queued map[string][]policy.Pending
	// Claimed are the tasks claimed upstream that no pod has picked up yet,
	// oldest first.
	Claimed []ClaimedView
}

// ClaimedView is a task claimed upstream that no pod has picked up yet.
type ClaimedView struct {
	ID, Tenant string
	// PodID is the pod it was placed on, empty until the policy placed it.
	PodID     string
	Email     string
	ClaimedAt time.Time
}

// View lets time pass first, so a hand-back it causes shows, and reports the
// core's state.
func (c *Core) View() View {
	c.Touch()
	live, _ := c.livePods()
	c.mu.Lock()
	defer c.mu.Unlock()
	v := View{Pods: map[string]PodView{}, Shares: c.policy.Shares(), LivePods: live, Queued: map[string][]policy.Pending{}}
	placed := map[string]string{}
	for id, pd := range c.held {
		pv := PodView{Held: true, Initiators: initiatorsOf(pd)}
		if l := firstLease(pd); l != nil {
			pv.Tenant = l.Tenant
		}
		if info, ok := c.policy.Pod(id); ok {
			pv.Policy = &info
		}
		pv.Busy = (pv.Policy != nil && pv.Policy.Phase != policy.PhaseWarm) || len(pd.leases) > 0 || len(pd.queue) > 0
		v.Pods[id] = pv
		for _, t := range pd.queue {
			placed[t.ID] = id
			v.Claimed = append(v.Claimed, ClaimedView{ID: t.ID, Tenant: t.Tenant, PodID: id, Email: t.Email, ClaimedAt: t.ClaimedAt})
		}
	}
	for id, t := range c.claimed {
		if _, ok := placed[id]; !ok {
			v.Claimed = append(v.Claimed, ClaimedView{ID: id, Tenant: t.Tenant, Email: t.Email, ClaimedAt: t.ClaimedAt})
		}
	}
	slices.SortFunc(v.Claimed, func(a, b ClaimedView) int {
		return cmp.Or(a.ClaimedAt.Compare(b.ClaimedAt), cmp.Compare(a.ID, b.ID))
	})
	for id, rec := range c.tenants {
		v.Tenants = append(v.Tenants, TenantView{ID: id, Leased: c.leasedLocked(id), Busy: c.tenantBusyLocked(id), Draining: rec.removed})
		if q := c.policy.Queued(id); len(q) > 0 {
			v.Queued[id] = q
		}
	}
	for _, l := range c.leases {
		if v.OldestLease.IsZero() || l.StartedAt.Before(v.OldestLease) {
			v.OldestLease = l.StartedAt
		}
	}
	return v
}
