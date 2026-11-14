package manager

import (
	"fmt"
	"strings"
	"time"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/multica"

	"github.com/google/uuid"
)

// DefaultUpstreamBackoff is how long a workspace whose upstream failed is left
// alone before the manager claims from it again.
const DefaultUpstreamBackoff = 10 * time.Second

// WorkspaceConfig is one workspace served by the manager; the manager finds
// them itself from the bot account's PAT (see Sync) and acts in each with
// that PAT. The pool is shared; the workspace is bound to a pod only for the
// duration of a lease.
type WorkspaceConfig struct {
	WorkspaceID string
	// RuntimeName is the runtime name members see in this workspace.
	RuntimeName string
	// Quota is the most slots the workspace's running tasks may occupy at once; 0 means no cap.
	Quota int
	// Placement is the workspace's affinity rule (see Config). Empty
	// inherits the pool's Placement.
	AffinityRule lending.AffinityRule
	// Slug is the workspace's slug, which overrides are keyed by.
	Slug string
	// HoldTimeout and MaxLifetime are the workspace's own; nil inherits the
	// pool's HoldTimeout and MaxLifetime.
	HoldTimeout *time.Duration
	MaxLifetime *time.Duration
}

func (w *WorkspaceConfig) normalize(defaultName string) error {
	if _, err := uuid.Parse(w.WorkspaceID); err != nil {
		return fmt.Errorf("workspace ID must be a UUID")
	}
	w.WorkspaceID = strings.ToLower(w.WorkspaceID)
	if w.Quota < 0 {
		return fmt.Errorf("workspace %s: quota must not be negative", w.WorkspaceID)
	}
	if err := multica.CheckAffinityRule(w.AffinityRule); err != nil {
		return fmt.Errorf("workspace %s: %w", w.WorkspaceID, err)
	}
	if w.RuntimeName == "" {
		w.RuntimeName = defaultName
	}
	return nil
}

// setSettings takes next's slug and settings, the ones a reload may change
// while the workspace is served, and reports whether a setting changed.
func (w *WorkspaceConfig) setSettings(next WorkspaceConfig) bool {
	changed := w.Quota != next.Quota || w.AffinityRule != next.AffinityRule ||
		!equalDuration(w.HoldTimeout, next.HoldTimeout) || !equalDuration(w.MaxLifetime, next.MaxLifetime)
	w.Slug, w.Quota, w.AffinityRule = next.Slug, next.Quota, next.AffinityRule
	w.HoldTimeout, w.MaxLifetime = next.HoldTimeout, next.MaxLifetime
	return changed
}

func equalDuration(a, b *time.Duration) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// workspace is the manager's live state for one configured workspace. Its
// runtime registration and connection (rt) are its own, so one workspace
// failing cannot touch another.
type workspace struct {
	cfg WorkspaceConfig
	rt  *multica.Runtimes

	// The fields below are guarded by Manager.mu.
	// removed is set when a reload drops the workspace; it is not claimed
	// from while set. A workspace that comes back before its drain ended is
	// served again with this same object, and drainGen tells the drain that
	// was started for an earlier removal to stop.
	removed  bool
	drainGen int
}

// UpstreamHealth is the outcome of a workspace's latest call to Multica.
type UpstreamHealth = multica.Health

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
	// Upstream is the latest claim, pending read or heartbeat outcome; nil
	// before the first.
	Upstream *UpstreamHealth `json:"upstream,omitempty"`
}

// workspaceBusyLocked reports whether the workspace holds anything in this
// manager: a lease, a claimed task not yet picked up, or a claim in flight.
// Removed and live objects of the same ID count alike. Callers hold p.mu.
func (p *Manager) workspaceBusyLocked(id string) bool {
	for _, l := range p.leases {
		if l.WorkspaceID == id {
			return true
		}
	}
	return p.policy.Busy(id)
}

// runtimeGone reports whether Multica said the runtime no longer exists.
func (p *Manager) runtimeGone(runtimeID string) bool {
	for _, ws := range p.ws.served() {
		if ws.rt.Gone(runtimeID) {
			return true
		}
	}
	return false
}

// dropRegistration forgets a workspace's registration after Multica answered
// that one of its runtimes is gone, and hints the pods to register the
// workspace again (see multica.Runtimes.Drop).
func (p *Manager) dropRegistration(ws *workspace, runtimeID string) {
	if !ws.rt.Drop(runtimeID) {
		return
	}
	p.log.Warn("runtime gone upstream; the workspace registers again with the next pod", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", runtimeID)
	p.api.NotifyWorkspacesChangedLimited()
}

func (p *Manager) workspaceByID(id string) *workspace {
	return p.ws.get(id)
}

// workspaceByRuntime finds the workspace owning an upstream runtime ID.
func (p *Manager) workspaceByRuntime(id string) *workspace {
	for _, ws := range p.workspaceList() {
		if ws.rt.Owns(id) {
			return ws
		}
	}
	return nil
}

// leasedLocked counts the workspace's active leases. Callers hold p.mu.
func (p *Manager) leasedLocked(ws *workspace) int {
	n := 0
	for _, l := range p.leases {
		if l.ws == ws {
			n++
		}
	}
	return n
}
