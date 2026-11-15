package manager

import (
	_ "embed"
	"net/http"
	"time"
)

// The dispatch view: how the live pods are shared between the workspaces, as
// the lending policy aims to share them (the target) and as they are (the
// occupancy). GET /v1/dispatch returns it as JSON, and GET /dispatch serves a
// page that shows it and reads it again every ten seconds.

// DispatchView is the pool's sharing at one moment.
type DispatchView struct {
	At time.Time `json:"at"`
	// Pool counts the pods by state; Live are those that can be lent (not
	// lost), the pods the fair shares split.
	Pool DispatchPool `json:"pool"`
	// Workspaces lists each served workspace's share, then the draining
	// ones.
	Workspaces []DispatchWorkspace `json:"workspaces"`
	// Totals add up the workspaces.
	Totals DispatchTotals `json:"totals"`
	// Pods lists every pod, in registration order.
	Pods []DispatchPod `json:"pods"`
}

// DispatchPod is what one pod is doing.
type DispatchPod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// State is idle, leased, resetting or lost (see PodInfo).
	State string `json:"state"`
	// WorkspaceID is the workspace the pod is bound to; empty when none.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// Hold is set while its binding runs nothing; HoldRemainingSeconds is
	// how long before it is reset.
	Hold                 bool  `json:"hold,omitempty"`
	HoldRemainingSeconds int64 `json:"hold_remaining_seconds,omitempty"`
	// Kept is set for a binding of the workspace rule, whose hold has no end.
	Kept bool `json:"kept,omitempty"`
	// Tasks are the tasks its binding holds, and UsedSlots/CapacitySlots its
	// slot use.
	Tasks         int      `json:"tasks,omitempty"`
	UsedSlots     int      `json:"used_slots,omitempty"`
	CapacitySlots int      `json:"capacity_slots,omitempty"`
	Initiators    []string `json:"initiators,omitempty"`
}

// DispatchPool counts the pool's pods.
type DispatchPool struct {
	Expected  int `json:"expected"`
	Live      int `json:"live"`
	Idle      int `json:"idle"`
	Leased    int `json:"leased"`
	Resetting int `json:"resetting"`
	Lost      int `json:"lost"`
}

// DispatchWorkspace is one workspace's share: its settings, the target the
// policy computes for it, and what it occupies.
type DispatchWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug,omitempty"`
	Name        string `json:"name"`
	Draining    bool   `json:"draining,omitempty"`
	// Guarantee is as configured, GuaranteeInEffect scaled to the live pods.
	Guarantee         int `json:"guarantee"`
	GuaranteeInEffect int `json:"guarantee_in_effect"`
	Weight            int `json:"weight"`
	// Quota caps its running tasks in slots; 0 is no cap.
	Quota int `json:"quota"`
	// TargetPods is the policy's share for it of the live pods, for the work
	// there is now.
	TargetPods int `json:"target_pods"`
	// OccupiedPods are the pods it occupies: RunningPods with a task (or a
	// claim in flight) and HoldingPods in hold.
	OccupiedPods int `json:"occupied_pods"`
	RunningPods  int `json:"running_pods"`
	HoldingPods  int `json:"holding_pods"`
	// Leased counts its running tasks; Queued its tasks waiting upstream.
	Leased int `json:"leased"`
	Queued int `json:"queued"`
	// Tasks are its tasks: those waiting upstream in the order they will be
	// claimed, those claimed and not yet picked up by their pod, those
	// running, and the last few ended (see DispatchRecent).
	Tasks []DispatchTask `json:"tasks"`
}

// Task states in the dispatch view.
const (
	// TaskQueued waits upstream for a pod.
	TaskQueued = "queued"
	// TaskStarting is claimed and waits for its pod's daemon to pick it up.
	TaskStarting = "starting"
	// TaskRunning runs on a pod.
	TaskRunning = "running"
	// TaskEnded ended within DispatchRecent.
	TaskEnded = "ended"
)

// DispatchRecent is how long an ended task stays in the dispatch view, and
// DispatchRecentMax how many ended tasks a workspace shows at most.
const (
	DispatchRecent    = 15 * time.Minute
	DispatchRecentMax = 8
)

// DispatchTask is one task of a workspace.
type DispatchTask struct {
	ID    string `json:"id"`
	State string `json:"state"`
	// Priority is the server's priority, for a queued task.
	Priority int `json:"priority,omitempty"`
	// Since is when it entered its state: created upstream, claimed,
	// started, or ended.
	Since time.Time `json:"since"`
	// StartedAt is when an ended task started.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// PodID and PodName are its pod, once it has one.
	PodID     string `json:"pod_id,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
	Initiator string `json:"initiator,omitempty"`
	// EndReason is why an ended task ended.
	EndReason string `json:"end_reason,omitempty"`
}

// DispatchTotals add up the workspaces' shares.
type DispatchTotals struct {
	GuaranteeInEffect int `json:"guarantee_in_effect"`
	TargetPods        int `json:"target_pods"`
	OccupiedPods      int `json:"occupied_pods"`
	Leased            int `json:"leased"`
	Queued            int `json:"queued"`
	// UnoccupiedPods are the live pods no workspace occupies.
	UnoccupiedPods int `json:"unoccupied_pods"`
}

// Dispatch reports the pool's sharing now.
func (m *Manager) Dispatch() DispatchView {
	view := m.core.View()
	now := m.clock.Now()
	tasks := map[string][]DispatchTask{}
	for tenant, queued := range view.Queued {
		for _, q := range queued {
			tasks[tenant] = append(tasks[tenant], DispatchTask{ID: q.ID, State: TaskQueued, Priority: q.Priority, Since: q.CreatedAt.UTC()})
		}
	}
	for _, c := range view.Claimed {
		tasks[c.Tenant] = append(tasks[c.Tenant], DispatchTask{ID: c.ID, State: TaskStarting, Since: c.ClaimedAt.UTC(), PodID: c.PodID, Initiator: c.Email})
	}
	leases := m.core.Leases()
	ended := map[string][]DispatchTask{}
	for _, l := range leases {
		if l.EndedAt == nil {
			tasks[l.WorkspaceID] = append(tasks[l.WorkspaceID], DispatchTask{ID: l.TaskID, State: TaskRunning, Since: l.StartedAt.UTC(), PodID: l.PodID, Initiator: l.Initiator})
		}
	}
	for i := len(leases) - 1; i >= 0; i-- {
		l := leases[i]
		if l.EndedAt == nil || now.Sub(*l.EndedAt) > DispatchRecent || len(ended[l.WorkspaceID]) >= DispatchRecentMax {
			continue
		}
		started := l.StartedAt.UTC()
		ended[l.WorkspaceID] = append(ended[l.WorkspaceID], DispatchTask{ID: l.TaskID, State: TaskEnded, Since: l.EndedAt.UTC(), StartedAt: &started, PodID: l.PodID, Initiator: l.Initiator, EndReason: l.EndReason})
	}
	for ws, e := range ended {
		tasks[ws] = append(tasks[ws], e...)
	}
	return dispatchView(m.Inventory(), tasks, now)
}

// dispatchView reads the sharing out of an inventory.
func dispatchView(inv Inventory, tasks map[string][]DispatchTask, now time.Time) DispatchView {
	names := map[string]string{}
	for _, pd := range inv.Pods {
		names[pd.ID] = pd.Name
	}
	v := DispatchView{
		At: now.UTC(),
		Pool: DispatchPool{
			Expected: inv.ExpectedSize, Live: inv.LivePods,
			Idle: inv.Idle, Leased: inv.Leased, Resetting: inv.Resetting, Lost: inv.Lost,
		},
		Workspaces: []DispatchWorkspace{},
		Pods:       []DispatchPod{},
	}
	for _, pd := range inv.Pods {
		dp := DispatchPod{ID: pd.ID, Name: pd.Name, State: pd.State, WorkspaceID: pd.WorkspaceID, Initiators: pd.Initiators}
		if b := pd.Binding; b != nil {
			dp.WorkspaceID = b.WorkspaceID
			dp.Hold, dp.HoldRemainingSeconds, dp.Kept = b.Hold, b.HoldRemainingSeconds, b.Kept
			dp.Tasks, dp.UsedSlots, dp.CapacitySlots = b.Tasks, b.UsedSlots, b.CapacitySlots
		}
		v.Pods = append(v.Pods, dp)
	}
	add := func(ws WorkspaceInfo, draining bool) {
		w := DispatchWorkspace{
			WorkspaceID: ws.WorkspaceID, Slug: ws.Slug, Name: ws.Name, Draining: draining,
			Guarantee: ws.Guarantee, GuaranteeInEffect: ws.GuaranteeInEffect, Weight: ws.Weight, Quota: ws.Quota,
			TargetPods: ws.TargetPods, OccupiedPods: ws.OccupiedPods, HoldingPods: ws.HoldingPods,
			RunningPods: max(ws.OccupiedPods-ws.HoldingPods, 0), Leased: ws.Leased, Queued: ws.Queued,
			Tasks: []DispatchTask{},
		}
		for _, t := range tasks[ws.WorkspaceID] {
			if t.PodID != "" {
				t.PodName = names[t.PodID]
			}
			w.Tasks = append(w.Tasks, t)
		}
		v.Workspaces = append(v.Workspaces, w)
		v.Totals.GuaranteeInEffect += w.GuaranteeInEffect
		v.Totals.TargetPods += w.TargetPods
		v.Totals.OccupiedPods += w.OccupiedPods
		v.Totals.Leased += w.Leased
		v.Totals.Queued += w.Queued
	}
	for _, ws := range inv.Workspaces {
		add(ws, false)
	}
	for _, ws := range inv.Draining {
		add(ws, true)
	}
	v.Totals.UnoccupiedPods = max(v.Pool.Live-v.Totals.OccupiedPods, 0)
	return v
}

//go:embed dispatch.html
var dispatchPage []byte

// serveDispatchPage serves the page that shows /v1/dispatch.
func serveDispatchPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	_, _ = w.Write(dispatchPage)
}
