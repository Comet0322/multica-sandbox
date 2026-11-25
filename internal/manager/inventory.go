package manager

import (
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica/tasksource"
	"sandbox-manager/internal/policy"
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
	// HoldTimeout is the setting in effect, the pool default or the
	// workspace's override.
	HoldTimeout string `json:"hold_timeout"`
	// Guarantee and Weight are the workspace's fair-share settings;
	// GuaranteeInEffect is the guarantee scaled to the live pods.
	Guarantee         int `json:"guarantee"`
	GuaranteeInEffect int `json:"guarantee_in_effect"`
	Weight            int `json:"weight"`
	// OccupiedPods are the pods the workspace occupies (running, held or
	// idle), HoldingPods those of them held, IdlePods those idle,
	// TargetPods its fair share of the live pods for the work there is now,
	// and Queued its tasks waiting upstream.
	OccupiedPods int `json:"occupied_pods"`
	HoldingPods  int `json:"holding_pods"`
	IdlePods     int `json:"idle_pods"`
	TargetPods   int `json:"target_pods"`
	Queued       int `json:"queued"`
	// Upstream is the latest claim, pending read or heartbeat outcome; nil
	// before the first.
	Upstream *UpstreamHealth `json:"upstream,omitempty"`
}

// Inventory reports pod counts by phase and the pods themselves, in
// creation order: their state comes from the sandbox service, what they
// serve from the core, the workspaces from the task source.
func (m *Manager) Inventory() Inventory {
	view := m.core.View()
	pods := m.pool.Pods()
	health := m.pool.Health()
	inv := Inventory{
		MaxPods: m.maxPods, PrunedTotal: m.pool.PrunedTotal(), Problem: health.Problem, Broken: health.Broken,
		Failures: make([]PodFailure, 0, len(health.Failures)), FailureCounts: health.FailureCounts,
		Pods: []PodInfo{}, Workspaces: []WorkspaceInfo{}, LivePods: view.LivePods,
	}
	if why := m.core.StoreProblem(); why != "" {
		// Nothing is claimed or handed to a pod meanwhile, which matters more
		// than how pods start.
		inv.Problem = "the dispatch record cannot be written: " + why
	}
	for _, f := range health.Failures {
		inv.Failures = append(inv.Failures, PodFailure(f))
	}
	if inv.FailureCounts == nil {
		inv.FailureCounts = map[string]int{}
	}
	leased := map[string]int{}
	for _, t := range view.Tenants {
		leased[t.ID] = t.Leased
	}
	shares := map[string]policy.ShareInfo{}
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
	now := m.clock.Now()
	if !view.OldestLease.IsZero() {
		inv.OldestLeaseAgeSeconds = int64(now.Sub(view.OldestLease) / time.Second)
	}
	inv.PendingFailReports = m.core.PendingFailReports()
	for _, sp := range pods {
		pv := view.Pods[sp.ID]
		info := PodInfo{ID: sp.ID, Name: sp.Name, Phase: podPhase(sp, pv), CreatedAt: sp.CreatedAt, LastSeen: sp.LastSeen}
		switch info.Phase {
		case PodWarm:
			inv.Warm++
		case PodStarting:
			inv.Starting++
			info.Reason = sp.Reason
		case PodRunning:
			inv.Running++
		case PodHeld:
			inv.Held++
		case PodIdle:
			inv.Idle++
		case PodFailed:
			inv.Failed++
			info.Reason = sp.Reason
		}
		if pi := pv.Policy; pv.Held && pi != nil && pi.Phase != policy.PhaseWarm {
			info.WorkspaceID, info.Kind, info.Key, info.TaskID = pi.Tenant, string(pi.Kind), pi.Key, pi.Task
			info.Initiators = pv.Initiators
			switch pi.Phase {
			case policy.PhaseHeld:
				until := pi.Until
				info.HoldUntil = &until
				info.HoldRemainingSeconds = max(int64(until.Sub(now)/time.Second), 0)
			case policy.PhaseIdle:
				since := pi.Since
				info.IdleSince = &since
			}
		}
		inv.Pods = append(inv.Pods, info)
	}
	return inv
}

func (m *Manager) workspaceInfo(ws tasksource.WorkspaceView, leased map[string]int, shares map[string]policy.ShareInfo) WorkspaceInfo {
	hold := m.cfg.HoldTimeout
	if ws.Config.HoldTimeout != nil {
		hold = *ws.Config.HoldTimeout
	}
	id := ws.Config.WorkspaceID
	s := shares[id]
	return WorkspaceInfo{
		WorkspaceID: id, Slug: ws.Config.Slug, Name: ws.Config.RuntimeName,
		Leased: leased[id], Quota: ws.Config.Quota, Upstream: ws.Upstream, HoldTimeout: hold.String(),
		Guarantee: ws.Config.Guarantee, GuaranteeInEffect: s.GuaranteeInEffect, Weight: max(ws.Config.Weight, 1),
		OccupiedPods: s.Occupied, HoldingPods: s.Holding, IdlePods: s.Idle, TargetPods: s.Target, Queued: s.Queued,
	}
}

// podPhase is the phase of a pod the pool lists: starting, warm or failed
// as the sandbox service says, and for a pod the core holds what the policy
// says it serves.
func podPhase(sp core.PodInfo, pv core.PodView) string {
	switch sp.State {
	case core.PodStarting:
		return PodStarting
	case core.PodLost:
		return PodFailed
	case core.PodHeld:
		if pi := pv.Policy; pv.Held && pi != nil {
			switch pi.Phase {
			case policy.PhaseRunning:
				return PodRunning
			case policy.PhaseHeld:
				return PodHeld
			case policy.PhaseIdle:
				return PodIdle
			}
		}
	}
	return PodWarm
}
