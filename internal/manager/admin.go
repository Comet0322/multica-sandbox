package manager

import (
	"net/http"
	"time"
)

// The read-only admin API. It reads the manager through adminSource and knows
// no other module.

// adminSource is what the admin API reports.
type adminSource interface {
	Inventory() Inventory
	Leases() []LeaseInfo
	Dispatch() DispatchView
}

// newAdminHandler serves the read-only query interface for administrators. It
// must not be reachable from pods.
func newAdminHandler(src adminSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/inventory", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, src.Inventory())
	})
	mux.HandleFunc("GET /v1/leases", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, src.Leases())
	})
	mux.HandleFunc("GET /v1/dispatch", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, src.Dispatch())
	})
	mux.HandleFunc("GET /dispatch", serveDispatchPage)
	return mux
}

// PodInfo is one pod's entry in the inventory.
type PodInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Phase is warm, starting, running, held, idle or failed.
	Phase string `json:"phase"`
	// Kind, WorkspaceID and Key are what a pod that served a task serves:
	// its runtime kind, its workspace (department) and its task's key, or
	// the key it is held for. TaskID is the task placed or running on it.
	Kind        string `json:"kind,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Key         string `json:"key,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	// Initiators are the people whose tasks the pod currently holds, sorted.
	Initiators []string  `json:"initiators,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeen   time.Time `json:"last_seen"`
	// HoldUntil is when a held pod's hold ends; IdleSince when an idle pod
	// became idle.
	HoldUntil            *time.Time `json:"hold_until,omitempty"`
	HoldRemainingSeconds int64      `json:"hold_remaining_seconds,omitempty"`
	IdleSince            *time.Time `json:"idle_since,omitempty"`
	// Reason is why a failed pod failed, or why a starting pod still waits.
	Reason string `json:"reason,omitempty"`
}

// PodFailure is one pod failure: when, which pod, in which phase (create,
// start or run), what kind (oom, evicted, image, refused...), whether
// retrying helps, what happened and what to do.
type PodFailure struct {
	At        time.Time `json:"at"`
	Pod       string    `json:"pod,omitempty"`
	Phase     string    `json:"phase"`
	Kind      string    `json:"kind"`
	Permanent bool      `json:"permanent"`
	What      string    `json:"what"`
	Advice    string    `json:"advice,omitempty"`
}

// Inventory is the manager's view of the pods. Name is display-only.
type Inventory struct {
	// MaxPods is the budget (SANDBOX_MAX_PODS).
	MaxPods int `json:"max_pods"`
	// The pods by phase.
	Warm     int `json:"warm"`
	Starting int `json:"starting"`
	Running  int `json:"running"`
	Held     int `json:"held"`
	Idle     int `json:"idle"`
	Failed   int `json:"failed"`
	// PrunedTotal counts failed pods dropped from the inventory since the
	// manager started.
	PrunedTotal int `json:"pruned_total"`
	// Problem is why pods could not be created or started lately (no node
	// capacity, an image that cannot be pulled, refused credentials), empty
	// when none is known.
	Problem string `json:"problem,omitempty"`
	// Broken is why no pod starts at all (an image that cannot be pulled,
	// refused credentials, or SANDBOX_POD_START_FAILURES failures in a row),
	// empty while pods start. While broken, one pod at a time tries.
	Broken string `json:"broken,omitempty"`
	// Failures are the latest pod failures, newest first; FailureCounts
	// counts every pod failure since the manager started, by kind.
	Failures      []PodFailure   `json:"failures"`
	FailureCounts map[string]int `json:"failure_counts"`
	// Workspaces lists each served workspace with its lease count and cap.
	Workspaces []WorkspaceInfo `json:"workspaces"`
	// Draining lists workspaces removed by a reload that still hold leases;
	// they are no longer polled.
	Draining []WorkspaceInfo `json:"draining,omitempty"`
	Pods     []PodInfo       `json:"pods"`
	// LastSync is the outcome of the latest workspace listing; nil before the
	// first one or without discovery.
	LastSync *SyncStatus `json:"last_sync,omitempty"`
	// OldestLeaseAgeSeconds is the age of the oldest active lease, 0 with none.
	// A value near the maximum lease time points at a task nobody reports on.
	OldestLeaseAgeSeconds int64 `json:"oldest_lease_age_seconds"`
	// PendingFailReports counts failure reports Multica has not taken yet.
	PendingFailReports int `json:"pending_fail_reports"`
	// OverrideProblems lists the per-workspace overrides that are not
	// applied, and why: a bad value, an unknown slug, an unreadable file, a
	// setting no longer used.
	OverrideProblems []string `json:"override_problems,omitempty"`
	// LivePods is the number of pods that are not failed, which the fair
	// shares were last computed for.
	LivePods int `json:"live_pods"`
}
