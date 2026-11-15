package tasksource

import (
	"fmt"
	"strings"
	"time"

	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/policy"

	"github.com/google/uuid"
)

// WorkspaceConfig is one workspace served by the source; the source finds
// them itself from the bot account's PAT (see Sync) and acts in each with
// that PAT.
type WorkspaceConfig struct {
	WorkspaceID string
	// RuntimeName is the runtime name members see in this workspace.
	RuntimeName string
	// Quota is the most pods the workspace's running tasks may occupy at
	// once; 0 means no cap.
	Quota int
	// AffinityRule is the workspace's affinity rule, read but no longer
	// used (ADR 0009). Empty inherits the pool's.
	AffinityRule multica.AffinityRule
	// Slug is the workspace's slug, which overrides are keyed by.
	Slug string
	// HoldTimeout is the workspace's own; nil inherits the pool's.
	// MaxLifetime is read but no longer used (ADR 0009).
	HoldTimeout *time.Duration
	MaxLifetime *time.Duration
	// Guarantee is the number of pods kept for the workspace; Weight shares
	// out the pods beyond the guarantees, 0 counting as 1 (see
	// policy.TenantSettings).
	Guarantee int
	Weight    int
}

func (w *WorkspaceConfig) normalize(defaultName string, defaultRule multica.AffinityRule) error {
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

// Settings are the workspace's policy settings. Its affinity rule and
// maximum lifetime are no longer among them (ADR 0009).
func (w WorkspaceConfig) Settings() policy.TenantSettings {
	return policy.TenantSettings{Quota: w.Quota, HoldTimeout: w.HoldTimeout, Guarantee: w.Guarantee, Weight: w.Weight}
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
// registrations and connections (one set per runtime kind) are its own, so
// one workspace failing cannot touch another.
type workspace struct {
	cfg  WorkspaceConfig
	sets []kindSet
	// removed is set when the workspace was dropped; it stays until the core
	// closes it (its drain ended), and is served again as the same object if
	// it comes back first.
	removed bool
}

// kindSet is a workspace's runtimes of one kind.
type kindSet struct {
	kind policy.Kind
	rt   *multica.Runtimes
}

// conn is the workspace's connection, for what concerns no runtime kind
// (failing a task, reading its status): every set acts with the same PAT.
func (w *workspace) conn() *multica.Conn { return w.sets[0].rt.Conn() }

// of is the workspace's runtimes of a kind, nil when the kind is off.
func (w *workspace) of(kind policy.Kind) *multica.Runtimes {
	for _, set := range w.sets {
		if set.kind == kind {
			return set.rt
		}
	}
	return nil
}

// owning is the workspace's runtimes that own a runtime ID, nil when none.
func (w *workspace) owning(runtimeID string) *multica.Runtimes {
	for _, set := range w.sets {
		if set.rt.Owns(runtimeID) {
			return set.rt
		}
	}
	return nil
}

// health is the latest outcome among the workspace's sets; nil before any.
func (w *workspace) health() *multica.Health {
	var latest *multica.Health
	for _, set := range w.sets {
		if h := set.rt.Health(); h != nil && (latest == nil || h.At.After(latest.At)) {
			latest = h
		}
	}
	return latest
}

// close closes the workspace's connections.
func (w *workspace) close() {
	for _, set := range w.sets {
		set.rt.Conn().Close()
	}
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
