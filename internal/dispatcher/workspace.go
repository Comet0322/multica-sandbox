package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"sandbox-manager/internal/lending"
)

// DefaultUpstreamBackoff is how long a workspace whose upstream failed is left
// alone before the manager claims from it again.
const DefaultUpstreamBackoff = 10 * time.Second

// upstreamClaimTimeout bounds one claim call so a hanging workspace upstream
// delays the other workspaces' claims by at most this long.
const upstreamClaimTimeout = 10 * time.Second

// WorkspaceConfig is one workspace served by the manager; the manager finds
// them itself from the bot account's ticket (see Sync) and acts in each with
// that ticket. The pool is shared; the workspace is bound to a pod only for the
// duration of a lease.
type WorkspaceConfig struct {
	WorkspaceID string
	// RuntimeName is the runtime name members see in this workspace.
	RuntimeName string
	// Quota is the most slots the workspace's running tasks may occupy at once; 0 means no cap.
	Quota int
	// Placement is the workspace's affinity rule (see PoolConfig). Empty
	// inherits the pool's Placement.
	Placement lending.AffinityRule
	// Slug is the workspace's slug, which overrides are keyed by.
	Slug string
	// HoldTimeout and MaxLifetime are the workspace's own; nil inherits the
	// pool's HoldTimeout and PodMaxLifetime.
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
	if err := CheckPlacement(w.Placement); err != nil {
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
	changed := w.Quota != next.Quota || w.Placement != next.Placement ||
		!equalDuration(w.HoldTimeout, next.HoldTimeout) || !equalDuration(w.MaxLifetime, next.MaxLifetime)
	w.Slug, w.Quota, w.Placement = next.Slug, next.Quota, next.Placement
	w.HoldTimeout, w.MaxLifetime = next.HoldTimeout, next.MaxLifetime
	return changed
}

func equalDuration(a, b *time.Duration) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// perWorkspaceManagerID is the daemon_id the manager used to derive for each
// workspace. Registration sends it as a legacy id so the server merges those
// runtime rows into the one identity's.
func perWorkspaceManagerID(multicaURL, workspaceID string) string {
	sum := sha256.Sum256([]byte(multicaURL + "\x00" + workspaceID))
	// The prefix is the id the program registered under before it was
	// renamed.
	return "sandbox-broker-" + hex.EncodeToString(sum[:])[:24]
}

// workspace is the manager's live state for one configured workspace: its own
// upstream connection and runtime registration, so one workspace failing
// cannot touch another.
type workspace struct {
	cfg WorkspaceConfig
	up  *upstream
	// registerMu serializes this workspace's upstream registration.
	registerMu sync.Mutex

	// The fields below are guarded by Pool.mu.
	registration json.RawMessage
	// removed is set when a reload drops the workspace; it is not claimed
	// from while set. A workspace that comes back before its drain ended is
	// served again with this same object, and drainGen tells the drain that
	// was started for an earlier removal to stop.
	removed  bool
	drainGen int
	// goneRuntimes are runtime IDs Multica said no longer exist; a pod that
	// heartbeats one is told so, which makes its daemon register again.
	goneRuntimes map[string]bool
	// shared is set once the registered runtimes were seen with the wanted
	// visibility; until then every Sync tries again.
	shared bool
	// health is the outcome of the latest claim or heartbeat upstream.
	health *UpstreamHealth
}

// UpstreamHealth is the outcome of a workspace's latest call to Multica.
type UpstreamHealth struct {
	OK bool `json:"ok"`
	// Op is the call: "claim" or "heartbeat".
	Op    string    `json:"op"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

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
	Placement   string `json:"placement"`
	HoldTimeout string `json:"hold_timeout"`
	MaxLifetime string `json:"max_lifetime"`
	// Upstream is the latest claim or heartbeat outcome; nil before the first.
	Upstream *UpstreamHealth `json:"upstream,omitempty"`
}

// healthInfo copies the workspace's upstream health. Callers hold Pool.mu.
func (ws *workspace) healthInfo() *UpstreamHealth {
	if ws.health == nil {
		return nil
	}
	h := *ws.health
	return &h
}

// recordUpstream keeps the outcome of a claim or heartbeat for the inventory.
// The error text is the upstream error's, which carries no response body.
func (p *Pool) recordUpstream(ws *workspace, op string, err error) {
	h := &UpstreamHealth{OK: err == nil, Op: op, At: p.now()}
	if err != nil {
		h.Error = boundText(err.Error(), 300)
	}
	p.mu.Lock()
	ws.health = h
	p.mu.Unlock()
}

// workspaceBusyLocked reports whether the workspace holds anything in this
// manager: a lease, a claimed task not yet picked up, or a claim in flight.
// Removed and live objects of the same ID count alike. Callers hold p.mu.
func (p *Pool) workspaceBusyLocked(id string) bool {
	for _, l := range p.leases {
		if l.WorkspaceID == id {
			return true
		}
	}
	return p.policy.Busy(id)
}

// runtimeGoneLocked reports whether Multica said the runtime no longer exists.
// Callers hold p.mu.
func (p *Pool) runtimeGoneLocked(runtimeID string) bool {
	if runtimeID == "" {
		return false
	}
	for _, ws := range p.ws.served() {
		if ws.goneRuntimes[runtimeID] {
			return true
		}
	}
	return false
}

// dropRegistration forgets a workspace's upstream registration after Multica
// answered that one of its runtimes is gone (deleted in the UI, or collected
// after a long time offline). The next pod registration for the workspace
// registers it upstream again; pods heartbeating the gone runtime are told so
// (serveHeartbeat), which is what makes the stock daemon register again.
func (p *Pool) dropRegistration(ws *workspace, runtimeID string) {
	p.mu.Lock()
	if !slices.Contains(p.runtimeIDsLocked(ws), runtimeID) {
		p.mu.Unlock()
		return // already dropped, or registered again since
	}
	if ws.goneRuntimes == nil {
		ws.goneRuntimes = map[string]bool{}
	}
	ws.goneRuntimes[runtimeID] = true
	ws.registration = nil
	ws.shared = false
	p.mu.Unlock()
	p.log.Warn("runtime gone upstream; the workspace registers again with the next pod", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", runtimeID)
	p.api.notifyWorkspacesChangedLimited()
}

func (p *Pool) workspaceByID(id string) *workspace {
	return p.ws.get(id)
}

// workspaceByRuntime finds the workspace owning an upstream runtime ID.
func (p *Pool) workspaceByRuntime(id string) *workspace {
	for _, ws := range p.workspaceList() {
		for _, rid := range p.runtimeIDs(ws) {
			if rid == id {
				return ws
			}
		}
	}
	return nil
}

// leasedLocked counts the workspace's active leases. Callers hold p.mu.
func (p *Pool) leasedLocked(ws *workspace) int {
	n := 0
	for _, l := range p.leases {
		if l.ws == ws {
			n++
		}
	}
	return n
}

// runtimeIDsLocked lists the upstream runtime IDs of a workspace's
// registration. Callers hold p.mu.
func (p *Pool) runtimeIDsLocked(ws *workspace) []string {
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	_ = json.Unmarshal(ws.registration, &resp)
	ids := make([]string, 0, len(resp.Runtimes))
	for _, rt := range resp.Runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

func (p *Pool) runtimeIDs(ws *workspace) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtimeIDsLocked(ws)
}
