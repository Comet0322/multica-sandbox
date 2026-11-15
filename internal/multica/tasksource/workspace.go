package tasksource

import (
	"fmt"
	"strings"
	"time"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/multica"

	"github.com/google/uuid"
)

// WorkspaceConfig is one workspace served by the source; the source finds
// them itself from the bot account's PAT (see Sync) and acts in each with
// that PAT.
type WorkspaceConfig struct {
	WorkspaceID string
	// RuntimeName is the runtime name members see in this workspace.
	RuntimeName string
	// Quota is the most slots the workspace's running tasks may occupy at
	// once; 0 means no cap.
	Quota int
	// AffinityRule is the workspace's affinity rule. Empty inherits the
	// pool's.
	AffinityRule lending.AffinityRule
	// Slug is the workspace's slug, which overrides are keyed by.
	Slug string
	// HoldTimeout and MaxLifetime are the workspace's own; nil inherits the
	// pool's.
	HoldTimeout *time.Duration
	MaxLifetime *time.Duration
	// Guarantee is the number of pods kept for the workspace; Weight shares
	// out the pods beyond the guarantees, 0 counting as 1 (see
	// lending.TenantSettings).
	Guarantee int
	Weight    int
}

func (w *WorkspaceConfig) normalize(defaultName string, defaultRule lending.AffinityRule) error {
	if _, err := uuid.Parse(w.WorkspaceID); err != nil {
		return fmt.Errorf("workspace ID must be a UUID")
	}
	w.WorkspaceID = strings.ToLower(w.WorkspaceID)
	if w.Quota < 0 {
		return fmt.Errorf("workspace %s: quota must not be negative", w.WorkspaceID)
	}
	if w.Guarantee < 0 || w.Weight < 0 {
		return fmt.Errorf("workspace %s: guarantee and weight must not be negative", w.WorkspaceID)
	}
	if w.AffinityRule == "" {
		w.AffinityRule = defaultRule
	}
	if err := multica.CheckAffinityRule(w.AffinityRule); err != nil {
		return fmt.Errorf("workspace %s: %w", w.WorkspaceID, err)
	}
	if w.RuntimeName == "" {
		w.RuntimeName = defaultName
	}
	return nil
}

// Settings are the workspace's lending settings.
func (w WorkspaceConfig) Settings() lending.TenantSettings {
	return lending.TenantSettings{
		Quota: w.Quota, Rule: multica.LendingRule(w.AffinityRule), HoldTimeout: w.HoldTimeout, MaxLifetime: w.MaxLifetime,
		Guarantee: w.Guarantee, Weight: w.Weight,
	}
}

// setSettings takes next's slug and settings, the ones a reload may change
// while the workspace is served, and reports whether a setting changed.
func (w *WorkspaceConfig) setSettings(next WorkspaceConfig) bool {
	changed := w.Quota != next.Quota || w.AffinityRule != next.AffinityRule ||
		!equalDuration(w.HoldTimeout, next.HoldTimeout) || !equalDuration(w.MaxLifetime, next.MaxLifetime) ||
		w.Guarantee != next.Guarantee || w.Weight != next.Weight
	w.Slug, w.Quota, w.AffinityRule = next.Slug, next.Quota, next.AffinityRule
	w.HoldTimeout, w.MaxLifetime = next.HoldTimeout, next.MaxLifetime
	w.Guarantee, w.Weight = next.Guarantee, next.Weight
	return changed
}

func equalDuration(a, b *time.Duration) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// workspace is the source's live state for one served workspace. Its runtime
// registration and connection (rt) are its own, so one workspace failing
// cannot touch another.
type workspace struct {
	cfg WorkspaceConfig
	rt  *multica.Runtimes
	// removed is set when the workspace was dropped; it stays until the core
	// closes it (its drain ended), and is served again as the same object if
	// it comes back first.
	removed bool
}

// ReloadResult lists the workspace IDs a reload touched.
type ReloadResult struct {
	Added   []string
	Removed []string
	// Updated had its quota, affinity rule, hold timeout, maximum lifetime,
	// guarantee or weight changed, which applies to claims and placements
	// from now on.
	Updated []string
	// Unapplied had its runtime name changed, which needs a restart; the
	// running name stays in effect.
	Unapplied []string
}
