package manager_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/policy"

	"k8s.io/client-go/kubernetes/fake"
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
	cfg.MulticaURL, cfg.PAT, cfg.Name = up.URL, patPool, "Shared Pool"
	cfg.UpstreamBackoff = time.Minute
	cfg.ReadOverrides = file.read
	if len(cfg.Runtimes) == 0 {
		clean(&cfg)
	}
	return startManager(t, fake.NewSimpleClientset(), cfg, botPods, botPods, func(ctx context.Context, m *manager.Manager) error {
		_, err := m.Sync(ctx)
		return err
	})
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
	file.set("alpha:\n  quota: 1\n  hold_timeout: 5m\n")
	h := newOverrideHarness(t, up, file, manager.Config{Quota: 4, HoldTimeout: time.Hour})

	inv := h.inventory()
	got := workspaceInfo(t, inv, wsA)
	if got.Slug != "alpha" || got.Quota != 1 || got.HoldTimeout != "5m0s" {
		t.Fatalf("alpha does not show its overrides: %+v", got)
	}
	got = workspaceInfo(t, inv, wsB)
	if got.Slug != "beta" || got.Quota != 4 || got.HoldTimeout != "1h0m0s" {
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
	h := newOverrideHarness(t, up, file, manager.Config{HoldTimeout: time.Hour, Runtimes: []policy.Kind{policy.KindPool}})
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

	phases := map[string]string{}
	for _, p := range h.inventory().Pods {
		phases[p.ID] = p.Phase
	}
	if phases[pb.id] != manager.PodHeld {
		t.Fatalf("beta's pod should hold for the pool's hour: %v", phases)
	}
	if phases[pa.id] != manager.PodIdle {
		t.Fatalf("alpha's hold timeout of 0 should make its pod idle at once: %v", phases)
	}
}

// An overrides file written for the GitOps pool still loads: each entry that
// names a removed setting gives one warning, once, and its other settings
// apply.
func TestAnOldOverridesFileLoadsWithOneWarning(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA, slug: "alpha"}, &fakeWorkspace{id: wsB, slug: "beta"}
	up := newMultiMultica(t, a, b)
	file := &overridesFile{}
	file.set("alpha: {quota: 1, rule: person-issue}\nbeta: {quota: 2}\n")
	h := newOverrideHarness(t, up, file, manager.Config{Quota: 4})
	h.sync()

	inv := h.inventory()
	if got := workspaceInfo(t, inv, wsA); got.Quota != 1 {
		t.Fatalf("alpha's quota was not applied: %+v", got)
	}
	if got := workspaceInfo(t, inv, wsB); got.Quota != 2 {
		t.Fatalf("beta's quota was not applied: %+v", got)
	}
	if len(inv.OverrideProblems) != 1 || !strings.Contains(inv.OverrideProblems[0], `"alpha": rule is no longer used and is ignored`) {
		t.Fatalf("want one warning naming rule, got %q", inv.OverrideProblems)
	}
	if n := strings.Count(h.logs.String(), "rule is no longer used and is ignored"); n != 1 {
		t.Fatalf("the warning was logged %d times over two syncs, want once:\n%s", n, h.logs.String())
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

	file.set("alpha: {quota: 1}\n")
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
