package core

import (
	"time"

	"sandbox-manager/internal/lending"
)

// What the core reports for the inventory. The manager joins it with what the
// source and the pool report.

// PodView is what the core knows of one pod.
type PodView struct {
	// Held is set while the core holds the pod; Busy while it is bound or
	// holds a task.
	Held, Busy bool
	// Tenant is the tenant of the pod's longest-held lease, empty with none.
	Tenant string
	// Initiators are the people whose tasks the pod holds, sorted.
	Initiators []string
	// Binding is the pod's binding, nil when it has none.
	Binding *lending.BindingInfo
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
	// Shares are the tenants' fair shares (see lending.Policy.Shares).
	Shares []lending.ShareInfo
	// LivePods is the number of live pods the shares were computed for.
	LivePods int
	// OldestLease is when the oldest active lease started, zero with none.
	OldestLease time.Time
}

// View lets time pass first, so a hand-back it causes shows, and reports the
// core's state.
func (c *Core) View() View {
	c.Touch()
	live := c.livePods()
	c.mu.Lock()
	defer c.mu.Unlock()
	v := View{Pods: map[string]PodView{}, Shares: c.policy.Shares(), LivePods: live}
	for id, pd := range c.held {
		pv := PodView{Held: true, Initiators: initiatorsOf(pd)}
		if l := firstLease(pd); l != nil {
			pv.Tenant = l.Tenant
		}
		if b, ok := c.policy.Binding(id); ok {
			pv.Binding = &b
		}
		pv.Busy = pv.Binding != nil || len(pd.leases) > 0 || len(pd.queue) > 0
		v.Pods[id] = pv
	}
	for id, rec := range c.tenants {
		v.Tenants = append(v.Tenants, TenantView{ID: id, Leased: c.leasedLocked(id), Busy: c.tenantBusyLocked(id), Draining: rec.removed})
	}
	for _, l := range c.leases {
		if v.OldestLease.IsZero() || l.StartedAt.Before(v.OldestLease) {
			v.OldestLease = l.StartedAt
		}
	}
	return v
}
