package manager

import (
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/multica/tasksource"
)

// WorkspaceInfo is one workspace's entry in the inventory.
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	// Slug is empty until an overrides file asks for slugs.
	Slug   string `json:"slug,omitempty"`
	Name   string `json:"name"`
	Leased int    `json:"leased"`
	// Quota is 0 when the workspace has no cap.
	Quota int `json:"quota"`
	// Placement, HoldTimeout and MaxLifetime are the settings in effect, the
	// pool defaults or the workspace's overrides. A maximum lifetime of "0s"
	// means none.
	AffinityRule string `json:"placement"`
	HoldTimeout  string `json:"hold_timeout"`
	MaxLifetime  string `json:"max_lifetime"`
	// Guarantee and Weight are the workspace's fair-share settings;
	// GuaranteeInEffect is the guarantee scaled to the live pods.
	Guarantee         int `json:"guarantee"`
	GuaranteeInEffect int `json:"guarantee_in_effect"`
	Weight            int `json:"weight"`
	// OccupiedPods are the pods the workspace occupies (running or in hold),
	// HoldingPods those of them in hold, TargetPods its fair share of the
	// live pods for the work there is now, and Queued its tasks waiting
	// upstream.
	OccupiedPods int `json:"occupied_pods"`
	HoldingPods  int `json:"holding_pods"`
	TargetPods   int `json:"target_pods"`
	Queued       int `json:"queued"`
	// Upstream is the latest claim, pending read or heartbeat outcome; nil
	// before the first.
	Upstream *UpstreamHealth `json:"upstream,omitempty"`
}

// Inventory reports pod counts by state and the pods themselves, in
// registration order: their state comes from the pool, their bindings and
// leases from the core, the workspaces from the task source.
func (m *Manager) Inventory() Inventory {
	view := m.core.View()
	pods := m.pool.Pods()
	inv := Inventory{ExpectedSize: m.cfg.ExpectedSize, PrunedTotal: m.pool.PrunedTotal(), Pods: []PodInfo{}, Workspaces: []WorkspaceInfo{}, LivePods: view.LivePods}
	leased := map[string]int{}
	for _, t := range view.Tenants {
		leased[t.ID] = t.Leased
	}
	shares := map[string]lending.ShareInfo{}
	for _, s := range view.Shares {
		shares[s.Tenant] = s
	}
	served, draining := m.source.Workspaces()
	for _, ws := range served {
		inv.Workspaces = append(inv.Workspaces, m.workspaceInfo(ws, leased, shares))
	}
	for _, ws := range draining {
		inv.Draining = append(inv.Draining, m.workspaceInfo(ws, leased, shares))
	}
	inv.OverrideProblems = m.source.OverrideProblems()
	inv.LastSync = m.source.LastSync()
	if !view.OldestLease.IsZero() {
		inv.OldestLeaseAgeSeconds = int64(m.clock.Now().Sub(view.OldestLease) / time.Second)
	}
	inv.PendingFailReports = m.source.PendingFailReports()
	now := m.clock.Now()
	for _, sp := range pods {
		pv := view.Pods[sp.ID]
		info := PodInfo{ID: sp.ID, Name: sp.Name, State: podState(sp, pv), RegisteredAt: sp.RegisteredAt, LastSeen: sp.LastSeen}
		switch info.State {
		case PodIdle:
			inv.Idle++
		case PodLeased:
			inv.Leased++
		case PodResetting:
			inv.Resetting++
		case PodLost:
			inv.Lost++
		}
		if pv.Held {
			info.WorkspaceID = pv.Tenant
			info.Initiators = pv.Initiators
			info.Binding = m.podBinding(pv.Binding, now)
		}
		inv.Pods = append(inv.Pods, info)
	}
	return inv
}

func (m *Manager) workspaceInfo(ws tasksource.WorkspaceView, leased map[string]int, shares map[string]lending.ShareInfo) WorkspaceInfo {
	hold, life := m.cfg.HoldTimeout, m.cfg.MaxLifetime
	if ws.Config.HoldTimeout != nil {
		hold = *ws.Config.HoldTimeout
	}
	if ws.Config.MaxLifetime != nil {
		life = *ws.Config.MaxLifetime
	}
	id := ws.Config.WorkspaceID
	s := shares[id]
	return WorkspaceInfo{
		WorkspaceID: id, Slug: ws.Config.Slug, Name: ws.Config.RuntimeName,
		Leased: leased[id], Quota: ws.Config.Quota, Upstream: ws.Upstream,
		AffinityRule: string(ws.Config.AffinityRule), HoldTimeout: hold.String(), MaxLifetime: life.String(),
		Guarantee: ws.Config.Guarantee, GuaranteeInEffect: s.GuaranteeInEffect, Weight: max(ws.Config.Weight, 1),
		OccupiedPods: s.Occupied, HoldingPods: s.Holding, TargetPods: s.Target, Queued: s.Queued,
	}
}

// podState is the inventory state of a pod the pool lists: a free pod is
// idle, a held one leased while it has a binding or a task and idle
// otherwise.
func podState(sp core.PodInfo, pv core.PodView) string {
	switch sp.State {
	case core.PodResetting:
		return PodResetting
	case core.PodLost:
		return PodLost
	case core.PodHeld:
		if pv.Held && pv.Busy {
			return PodLeased
		}
	}
	return PodIdle
}

// podBinding describes a pod's binding for the inventory, nil when it has
// none.
func (m *Manager) podBinding(b *lending.BindingInfo, now time.Time) *PodBinding {
	if b == nil {
		return nil
	}
	out := &PodBinding{
		ID: uint64(b.ID), AffinityKey: b.Key, WorkspaceID: b.Tenant, Tasks: b.Tasks,
		UsedSlots: b.Used.Slots, CapacitySlots: m.cfg.Capacity, Hold: b.Hold, Kept: b.Kept,
	}
	if b.Hold && !b.Kept {
		until := b.HoldUntil
		out.HoldUntil = &until
		out.HoldRemainingSeconds = max(int64(until.Sub(now)/time.Second), 0)
	}
	return out
}
