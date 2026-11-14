package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/multica"

	"gopkg.in/yaml.v3"
)

// WorkspaceOverride replaces pool defaults for one workspace. A field left out
// keeps the pool default. Pod capacity is pool-wide and cannot be overridden.
type WorkspaceOverride struct {
	// Placement is the workspace's affinity rule (SANDBOX_PLACEMENT).
	AffinityRule lending.AffinityRule `yaml:"placement"`
	// HoldTimeout is the hold timeout of the bindings its tasks open
	// (SANDBOX_HOLD_TIMEOUT); 0 resets a pod as soon as its last task ends.
	HoldTimeout *time.Duration `yaml:"hold_timeout"`
	// MaxLifetime is the maximum lifetime of the bindings its tasks open
	// (SANDBOX_POD_MAX_LIFETIME); 0 means none.
	MaxLifetime *time.Duration `yaml:"max_lifetime"`
	// Quota overrides SANDBOX_POOL_QUOTA; 0 means no cap.
	Quota *int `yaml:"quota"`
}

func (o WorkspaceOverride) check() error {
	if err := multica.CheckAffinityRule(o.AffinityRule); err != nil {
		return err
	}
	if o.HoldTimeout != nil && *o.HoldTimeout < 0 {
		return errors.New("hold_timeout must not be negative")
	}
	if o.MaxLifetime != nil && *o.MaxLifetime < 0 {
		return errors.New("max_lifetime must not be negative")
	}
	if o.Quota != nil && *o.Quota < 0 {
		return errors.New("quota must not be negative")
	}
	return nil
}

// ParseWorkspaceOverrides reads an overrides file: a YAML (or JSON) mapping
// from workspace slug to WorkspaceOverride. An entry with an unknown field or
// a bad value is left out and reported in problems, keyed by its slug; the
// other entries stand. err is set, and nothing is returned, when the file as a
// whole cannot be read.
func ParseWorkspaceOverrides(raw []byte) (overrides map[string]WorkspaceOverride, problems map[string]error, err error) {
	var entries map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, nil, fmt.Errorf("not a mapping from workspace slug to settings: %w", err)
	}
	overrides, problems = map[string]WorkspaceOverride{}, map[string]error{}
	for slug, node := range entries {
		o, err := decodeOverride(&node)
		if err != nil {
			problems[slug] = err
			continue
		}
		overrides[slug] = o
	}
	return overrides, problems, nil
}

func decodeOverride(node *yaml.Node) (WorkspaceOverride, error) {
	var o WorkspaceOverride
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return o, nil
	}
	if err := durationsAsText(node); err != nil {
		return o, err
	}
	// Re-encode the entry so a strict decoder can refuse unknown fields.
	raw, err := yaml.Marshal(node)
	if err != nil {
		return o, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&o); err != nil {
		return o, oneLine(err)
	}
	return o, o.check()
}

// durationsAsText checks the durations of an entry with time.ParseDuration,
// as the environment's are, and tags them as text: a bare 0 then means zero.
func durationsAsText(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if (key.Value != "hold_timeout" && key.Value != "max_lifetime") || value.Kind != yaml.ScalarNode || value.Tag == "!!null" {
			continue
		}
		if _, err := time.ParseDuration(value.Value); err != nil {
			return fmt.Errorf("%s must be a duration such as 10m: %w", key.Value, err)
		}
		value.Tag = "!!str"
	}
	return nil
}

// oneLine turns a YAML decoding error into one line for the log and the
// inventory.
func oneLine(err error) error {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	msg = strings.TrimPrefix(msg, "unmarshal errors:\n")
	var parts []string
	for _, line := range strings.Split(msg, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return errors.New(strings.Join(parts, "; "))
}

// overrideState is what the manager keeps of the overrides file between
// reads. Guarded by Manager.mu.
type overrideState struct {
	// bySlug are the overrides in effect: each slug's last valid entry.
	bySlug map[string]WorkspaceOverride
	// problems are the latest pass's findings, for the inventory and the log.
	problems []string
}

// resolveWorkspaceSettings fills each listed workspace's settings: the pool
// defaults, replaced by the override for its slug. The overrides file is read
// again on every pass, so an edited file applies on the next Sync. A file that
// cannot be read keeps the overrides in effect; an entry with a bad value keeps
// that slug's previous override, or the defaults. Problems are reported, and
// never stop a workspace from being served.
func (p *Manager) resolveWorkspaceSettings(ctx context.Context, list []WorkspaceConfig) {
	var problems []string
	p.mu.Lock()
	overrides := p.overrides.bySlug
	p.mu.Unlock()
	if p.cfg.ReadOverrides != nil {
		overrides, problems = p.readOverrides(overrides)
	}
	if len(overrides) > 0 {
		problems = append(problems, p.fillSlugs(ctx, list)...)
	}
	known := map[string]bool{}
	for i := range list {
		wc := &list[i]
		known[wc.Slug] = true
		wc.AffinityRule, wc.Quota, wc.HoldTimeout, wc.MaxLifetime = p.cfg.AffinityRule, p.cfg.Quota, nil, nil
		o, ok := overrides[wc.Slug]
		if !ok || wc.Slug == "" {
			continue
		}
		if o.AffinityRule != "" {
			wc.AffinityRule = o.AffinityRule
		}
		if o.Quota != nil {
			wc.Quota = *o.Quota
		}
		wc.HoldTimeout, wc.MaxLifetime = o.HoldTimeout, o.MaxLifetime
	}
	for slug := range overrides {
		if !known[slug] {
			problems = append(problems, fmt.Sprintf("override %q: the account serves no workspace with this slug", slug))
		}
	}
	sort.Strings(problems)

	p.mu.Lock()
	changed := !slices.Equal(p.overrides.problems, problems)
	p.overrides = overrideState{bySlug: overrides, problems: problems}
	p.mu.Unlock()
	if !changed {
		return
	}
	for _, msg := range problems {
		p.log.Warn("workspace override not applied", "problem", msg)
	}
	if len(problems) == 0 {
		p.log.Info("workspace overrides applied without problems", "overrides", len(overrides))
	}
}

// readOverrides reads the overrides file and returns the overrides to apply:
// current where the file cannot be read, each valid entry, and the current
// override of a slug whose entry is bad.
func (p *Manager) readOverrides(current map[string]WorkspaceOverride) (map[string]WorkspaceOverride, []string) {
	raw, err := p.cfg.ReadOverrides()
	if err != nil {
		return current, []string{fmt.Sprintf("overrides file: cannot read it (%v); the overrides in effect stay", err)}
	}
	parsed, bad, err := ParseWorkspaceOverrides(raw)
	if err != nil {
		return current, []string{fmt.Sprintf("overrides file: %v; the overrides in effect stay", err)}
	}
	var problems []string
	for slug, err := range bad {
		if prev, ok := current[slug]; ok {
			parsed[slug] = prev
			problems = append(problems, fmt.Sprintf("override %q: %v; its previous override stays", slug, err))
		} else {
			problems = append(problems, fmt.Sprintf("override %q: %v; the pool defaults apply", slug, err))
		}
	}
	return parsed, problems
}

// fillSlugs sets the slug of each listed workspace. The daemon listing has
// none on older servers, so the member listing is read for them; when that
// fails, a served workspace keeps the slug it had.
func (p *Manager) fillSlugs(ctx context.Context, list []WorkspaceConfig) []string {
	missing := slices.ContainsFunc(list, func(wc WorkspaceConfig) bool { return wc.Slug == "" })
	if !missing {
		return nil
	}
	slugs, err := p.up.WorkspaceSlugs(ctx)
	var problems []string
	if err != nil {
		problems = append(problems, fmt.Sprintf("workspace slugs: cannot list them (%v); workspaces keep the slugs they had", err))
	}
	for i := range list {
		wc := &list[i]
		if wc.Slug != "" {
			continue
		}
		if s, ok := slugs[strings.ToLower(wc.WorkspaceID)]; ok {
			wc.Slug = s
		} else if ws := p.ws.get(wc.WorkspaceID); ws != nil {
			wc.Slug = ws.cfg.Slug
		}
	}
	return problems
}

// overrideProblems lists the latest problems with the overrides. Callers hold
// p.mu.
func (p *Manager) overrideProblemsLocked() []string {
	return slices.Clone(p.overrides.problems)
}
