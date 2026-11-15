package manager

import (
	_ "embed"
	"net/http"
	"time"
)

// The dispatch view: how the live pods are shared between the workspaces, as
// the policy aims to share them (the target) and as they are (the
// occupancy). GET /v1/dispatch returns it as JSON, and GET /dispatch serves a
// page that shows it and reads it again every ten seconds.

// DispatchView is the pool's sharing at one moment.
type DispatchView struct {
	At time.Time `json:"at"`
	// Pool counts the pods by phase; Live are those not failed, the pods the
	// fair shares split.
	Pool DispatchPool `json:"pool"`
	// Workspaces lists each served workspace's share, then the draining
	// ones.
	Workspaces []DispatchWorkspace `json:"workspaces"`
	// Totals add up the workspaces.
	Totals DispatchTotals `json:"totals"`
	// Pods lists every pod, in creation order.
	Pods []DispatchPod `json:"pods"`
}

// DispatchPod is what one pod is doing.
type DispatchPod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Phase is warm, starting, running, held, idle or failed (see PodInfo).
	Phase string `json:"phase"`
	// Kind, WorkspaceID, Key and TaskID are what it serves, once it served
	// a task.
	Kind        string `json:"kind,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Key         string `json:"key,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	// HoldRemainingSeconds is how long a held pod stays held.
	HoldRemainingSeconds int64    `json:"hold_remaining_seconds,omitempty"`
	Initiators           []string `json:"initiators,omitempty"`
	// Reason is why a failed pod failed, or why a starting pod still waits.
	Reason string `json:"reason,omitempty"`
}

// DispatchPool counts the pods by phase, against the budget.
type DispatchPool struct {
	Max      int `json:"max"`
	Live     int `json:"live"`
	Warm     int `json:"warm"`
	Starting int `json:"starting"`
	Running  int `json:"running"`
	Held     int `json:"held"`
	Idle     int `json:"idle"`
	Failed   int `json:"failed"`
	// Problem is why pods could not be created or started lately, such as
	// no node capacity or an image that cannot be pulled.
	Problem string `json:"problem,omitempty"`
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
	// Quota caps its running tasks in pods; 0 is no cap.
	Quota int `json:"quota"`
	// TargetPods is the policy's share for it of the live pods, for the work
	// there is now.
	TargetPods int `json:"target_pods"`
	// OccupiedPods are the pods it occupies: RunningPods with a task (or a
	// claim in flight), HoldingPods in hold and IdlePods idle.
	OccupiedPods int `json:"occupied_pods"`
	RunningPods  int `json:"running_pods"`
	HoldingPods  int `json:"holding_pods"`
	IdlePods     int `json:"idle_pods"`
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
			Max: inv.MaxPods, Live: inv.LivePods, Warm: inv.Warm, Starting: inv.Starting,
			Running: inv.Running, Held: inv.Held, Idle: inv.Idle, Failed: inv.Failed, Problem: inv.Problem,
		},
		Workspaces: []DispatchWorkspace{},
		Pods:       []DispatchPod{},
	}
	for _, pd := range inv.Pods {
		dp := DispatchPod{
			ID: pd.ID, Name: pd.Name, Phase: pd.Phase, Kind: pd.Kind, WorkspaceID: pd.WorkspaceID, Key: pd.Key, TaskID: pd.TaskID,
			HoldRemainingSeconds: pd.HoldRemainingSeconds, Initiators: pd.Initiators, Reason: pd.Reason,
		}
		v.Pods = append(v.Pods, dp)
	}
	add := func(ws WorkspaceInfo, draining bool) {
		w := DispatchWorkspace{
			WorkspaceID: ws.WorkspaceID, Slug: ws.Slug, Name: ws.Name, Draining: draining,
			Guarantee: ws.Guarantee, GuaranteeInEffect: ws.GuaranteeInEffect, Weight: ws.Weight, Quota: ws.Quota,
			TargetPods: ws.TargetPods, OccupiedPods: ws.OccupiedPods, HoldingPods: ws.HoldingPods, IdlePods: ws.IdlePods,
			RunningPods: max(ws.OccupiedPods-ws.HoldingPods-ws.IdlePods, 0), Leased: ws.Leased, Queued: ws.Queued,
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
