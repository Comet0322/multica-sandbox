package manager

import (
	"net/http"
	"time"
)

// The Audit / inventory module: the audit log (audit.go) and the read-only
// admin API. The admin API reads the manager through adminSource and knows no
// other module.

// adminSource is what the admin API reports.
type adminSource interface {
	Inventory() Inventory
	Leases() []LeaseInfo
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
	return mux
}

// PodInfo is one pod's entry in the inventory.
type PodInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	// WorkspaceID is the workspace the pod is lent to; empty unless leased.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// Initiators are the people whose tasks the pod currently holds, sorted.
	Initiators   []string  `json:"initiators,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
	// Binding is the pod's binding, nil when it has none.
	Binding *PodBinding `json:"binding,omitempty"`
}

// PodBinding is a pod's binding in the inventory: the affinity key it
// serves, its capacity use and, in hold, its countdown.
type PodBinding struct {
	ID          uint64 `json:"id"`
	AffinityKey string `json:"affinity_key"`
	WorkspaceID string `json:"workspace_id"`
	// Tasks is how many tasks the binding holds, running or placed.
	Tasks         int `json:"tasks"`
	UsedSlots     int `json:"used_slots"`
	CapacitySlots int `json:"capacity_slots"`
	// Hold is set while no task runs; the pod is reset at HoldUntil unless a
	// task of its key comes first.
	Hold                 bool       `json:"hold"`
	HoldUntil            *time.Time `json:"hold_until,omitempty"`
	HoldRemainingSeconds int64      `json:"hold_remaining_seconds,omitempty"`
}

// Inventory is the manager's view of the pool. Name is display-only.
type Inventory struct {
	ExpectedSize int `json:"expected_size"`
	Idle         int `json:"idle"`
	Leased       int `json:"leased"`
	Resetting    int `json:"resetting"`
	Lost         int `json:"lost"`
	// PrunedTotal counts lost pods dropped from the inventory since the manager
	// started.
	PrunedTotal int `json:"pruned_total"`
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
	// applied, and why: a bad value, an unknown slug, an unreadable file.
	OverrideProblems []string `json:"override_problems,omitempty"`
}
