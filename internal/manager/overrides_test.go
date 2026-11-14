package manager_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/multica"
)

// overridesFile stands in for the operator's overrides file; the pool reads
// it on every Sync.
type overridesFile struct {
	mu   sync.Mutex
	text string
	err  error
}

func (f *overridesFile) set(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text, f.err = text, nil
}

func (f *overridesFile) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *overridesFile) read() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.text), f.err
}

// newOverrideHarness runs a pool under the bot account that serves every
// workspace the fake lists, with the overrides file, after one Sync.
func newOverrideHarness(t *testing.T, up *multiMultica, file *overridesFile, cfg manager.Config) *harness {
	t.Helper()
	logs, audit := &syncBuffer{}, &syncBuffer{}
	cfg.MulticaURL, cfg.PAT, cfg.Name, cfg.AuditLog = up.URL, patPool, "Shared Pool", audit
	cfg.UpstreamBackoff = time.Minute
	cfg.ReadOverrides = file.read
	pool, err := manager.New(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pool: pool, logs: logs, audit: audit}
	h.podAPI = httptest.NewServer(pool.PodHandler())
	h.admin = httptest.NewServer(pool.AdminHandler())
	t.Cleanup(h.podAPI.Close)
	t.Cleanup(h.admin.Close)
	h.sync()
	return h
}

func (h *harness) sync() manager.ReloadResult {
	h.t.Helper()
	res, err := h.pool.Sync(context.Background())
	if err != nil {
		h.t.Fatalf("sync: %v", err)
	}
	return res
}

func workspaceInfo(t *testing.T, inv manager.Inventory, id string) manager.WorkspaceInfo {
	t.Helper()
	for _, w := range inv.Workspaces {
		if w.WorkspaceID == id {
			return w
		}
	}
	t.Fatalf("workspace %s not in the inventory: %+v", id, inv.Workspaces)
	return manager.WorkspaceInfo{}
}

func TestWorkspaceWithoutAnOverrideUsesThePoolDefaults(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	file.set("alpha:\n  quota: 1\n  placement: task\n  hold_timeout: 5m\n  max_lifetime: 1h\n")
	h := newOverrideHarness(t, up, file, manager.Config{
		Quota: 4, AffinityRule: multica.RulePersonIssue, HoldTimeout: time.Hour, MaxLifetime: 3 * time.Hour,
	})

	inv := h.inventory()
	got := workspaceInfo(t, inv, wsA)
	if got.Slug != "alpha" || got.Quota != 1 || got.AffinityRule != "task" || got.HoldTimeout != "5m0s" || got.MaxLifetime != "1h0m0s" {
		t.Fatalf("alpha does not show its overrides: %+v", got)
	}
	got = workspaceInfo(t, inv, wsB)
	if got.Slug != "beta" || got.Quota != 4 || got.AffinityRule != "person-issue" || got.HoldTimeout != "1h0m0s" || got.MaxLifetime != "3h0m0s" {
		t.Fatalf("beta does not show the pool defaults: %+v", got)
	}
	if len(inv.OverrideProblems) != 0 {
		t.Fatalf("unexpected problems: %v", inv.OverrideProblems)
	}
}

func TestQuotaOverrideCapsOnlyItsWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	file.set("alpha: {quota: 1}\n")
	h := newOverrideHarness(t, up, file, manager.Config{Quota: 2})
	p1, p2, p3 := h.newPod("pool-0"), h.newPod("pool-1"), h.newPod("pool-2")
	ids1, ids2, ids3 := p1.startAll(), p2.startAll(), p3.startAll()

	up.enqueue(a, "ta1")
	up.enqueue(a, "ta2")
	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}
	if got := p2.claimFrom(ids2); len(got) != 0 {
		t.Fatalf("alpha is at its overridden quota of 1, got %v", got)
	}
	up.enqueue(b, "tb1")
	up.enqueue(b, "tb2")
	if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("want [tb1], got %v", got)
	}
	if got := p3.claimFrom(ids3); !slices.Equal(got, []string{"tb2"}) {
		t.Fatalf("beta keeps the pool quota of 2, got %v", got)
	}
}

func TestHoldOverrideAppliesToItsWorkspaceOnly(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	file.set("alpha: {hold_timeout: 0s}\n")
	h := newOverrideHarness(t, up, file, manager.Config{HoldTimeout: time.Hour})
	pa, pb := h.newPod("pool-0"), h.newPod("pool-1")
	idsA, idsB := pa.startAll(), pb.startAll()

	up.enqueueIssue(a, "ta1", "issue-1")
	if got := pa.claimFrom(idsA); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}
	up.enqueueIssue(b, "tb1", "issue-2")
	if got := pb.claimFrom(idsB); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("want [tb1], got %v", got)
	}
	pa.report("ta1", "complete")
	pb.report("tb1", "complete")

	states := map[string]string{}
	for _, p := range h.inventory().Pods {
		states[p.ID] = string(p.State)
		if p.ID == pb.id && (p.Binding == nil || !p.Binding.Hold) {
			t.Fatalf("beta's pod should hold for the pool's hour: %+v", p)
		}
	}
	if states[pa.id] != string(manager.PodResetting) {
		t.Fatalf("alpha's hold timeout of 0 should reset its pod at once: %v", states)
	}
}

func TestBadOrUnknownOverridesAreReportedAndDoNotStopOthers(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	file.set(strings.Join([]string{
		"alpha: {quota: 1}",
		"beta: {hold_timeout: soon}",
		"gamma: {quota: 2}",
		"delta: {quota: 2}",
		"epsilon: {placement: pod}",
	}, "\n"))
	h := newOverrideHarness(t, up, file, manager.Config{Quota: 3})

	inv := h.inventory()
	if got := workspaceInfo(t, inv, wsA); got.Quota != 1 {
		t.Fatalf("alpha's valid override was not applied: %+v", got)
	}
	if got := workspaceInfo(t, inv, wsB); got.Quota != 3 || got.HoldTimeout != "0s" {
		t.Fatalf("beta's bad override should leave the pool defaults: %+v", got)
	}
	for _, slug := range []string{"beta", "gamma", "delta", "epsilon"} {
		if !slices.ContainsFunc(inv.OverrideProblems, func(p string) bool { return strings.Contains(p, `"`+slug+`"`) }) {
			t.Errorf("no problem reported for %s: %v", slug, inv.OverrideProblems)
		}
		if !strings.Contains(h.logs.String(), slug) {
			t.Errorf("%s not in the log", slug)
		}
	}
	if len(inv.OverrideProblems) != 4 {
		t.Fatalf("want one problem per bad entry, got %v", inv.OverrideProblems)
	}
}

func TestAnUnreadableOverridesFileKeepsTheCurrentOverrides(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	file.set("alpha: {quota: 1}\n")
	h := newOverrideHarness(t, up, file, manager.Config{})

	for name, breakIt := range map[string]func(){
		"not YAML":   func() { file.set("alpha: [\n") },
		"read error": func() { file.fail(errFake) },
	} {
		breakIt()
		h.sync()
		inv := h.inventory()
		if got := workspaceInfo(t, inv, wsA); got.Quota != 1 {
			t.Fatalf("%s: alpha lost its override: %+v", name, got)
		}
		if len(inv.OverrideProblems) != 1 {
			t.Fatalf("%s: want the file reported, got %v", name, inv.OverrideProblems)
		}
	}
	file.set("")
	h.sync()
	if got := workspaceInfo(t, h.inventory(), wsA); got.Quota != 0 {
		t.Fatalf("an emptied file should drop the override: %+v", got)
	}
}

var errFake = &fakeError{}

type fakeError struct{}

func (*fakeError) Error() string { return "permission denied" }

func TestReloadAppliesChangedOverridesToNewPlacementsOnly(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	h := newOverrideHarness(t, up, file, manager.Config{})
	p1, p2 := h.newPod("pool-0"), h.newPod("pool-1")
	ids1, ids2 := p1.startAll(), p2.startAll()
	up.enqueue(a, "ta1")
	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}

	file.set("alpha: {quota: 1, max_lifetime: 1ns}\n")
	res := h.sync()
	if !slices.Equal(res.Updated, []string{wsA}) || len(res.Unapplied) != 0 {
		t.Fatalf("the override change should apply without a restart: %+v", res)
	}
	up.enqueue(a, "ta2")
	if got := p2.claimFrom(ids2); len(got) != 0 {
		t.Fatalf("the new quota of 1 applies to the next claim, got %v", got)
	}
	if leases := h.leases(); len(leases) != 1 || leases[0].TaskID != "ta1" {
		t.Fatalf("the running task was disturbed: %+v", leases)
	}
	if status := p1.report("ta1", "complete"); status != http.StatusOK {
		t.Fatalf("the running task's report: status %d", status)
	}
}
