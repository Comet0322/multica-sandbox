package manager_test

// Manager tests against a fake sandbox service: the fake decides which
// pods are free, records every Create and Delete, and the test sends its
// notifications itself.

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sandbox-manager/internal/policy"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/sandbox"
)

type fakeSandbox struct {
	mu     sync.Mutex
	order  []sandbox.PodID
	state  map[sandbox.PodID]sandbox.PodState
	tokens map[string]sandbox.PodID
	// calls records "create <id>", "create: no capacity" and
	// "delete <id> (<reason>)" in order.
	calls []string
	// afterCreate, when set, runs once Create has marked the pod held and
	// before it returns, as a notification arriving in between would.
	afterCreate func(id sandbox.PodID)
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{state: map[sandbox.PodID]sandbox.PodState{}, tokens: map[string]sandbox.PodID{}}
}

// add registers a free pod and returns its token.
func (f *fakeSandbox) add(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, sandbox.PodID(id))
	f.state[sandbox.PodID(id)] = sandbox.PodFree
	token := "mpt_" + id
	f.tokens[token] = sandbox.PodID(id)
	return token
}

// set changes a pod's state, as its start or its loss would.
func (f *fakeSandbox) set(id string, s sandbox.PodState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[sandbox.PodID(id)] = s
}

func (f *fakeSandbox) seen(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSandbox) Create(context.Context) (sandbox.PodID, error) {
	f.mu.Lock()
	for _, id := range f.order {
		if f.state[id] == sandbox.PodFree {
			f.state[id] = sandbox.PodHeld
			f.calls = append(f.calls, "create "+string(id))
			hook := f.afterCreate
			f.mu.Unlock()
			if hook != nil {
				hook(id)
			}
			return id, nil
		}
	}
	f.calls = append(f.calls, "create: no capacity")
	f.mu.Unlock()
	return "", sandbox.ErrNoCapacity
}

func (f *fakeSandbox) Delete(_ context.Context, id sandbox.PodID, label sandbox.AuditLabel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete "+string(id)+" ("+label.Reason+")")
	if f.state[id] == sandbox.PodHeld {
		// A new pod starts in its place, under the same ID here.
		f.state[id] = sandbox.PodStarting
	}
	return nil
}

func (f *fakeSandbox) Record(sandbox.PodID, sandbox.PodRecord) {}

func (f *fakeSandbox) Restore(context.Context) ([]sandbox.RestoredPod, error) { return nil, nil }

func (f *fakeSandbox) PutFiles(context.Context, sandbox.PodID, string, io.Reader) error {
	return sandbox.ErrUnsupported
}

func (f *fakeSandbox) GetFiles(context.Context, sandbox.PodID, string) (io.ReadCloser, error) {
	return nil, sandbox.ErrUnsupported
}

func (f *fakeSandbox) Exec(context.Context, sandbox.PodID, sandbox.Command) (sandbox.Result, error) {
	return sandbox.Result{}, sandbox.ErrUnsupported
}

func (f *fakeSandbox) Authenticate(token string) (sandbox.PodID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.tokens[token]
	return id, ok
}

func (f *fakeSandbox) Inventory() sandbox.Inventory {
	f.mu.Lock()
	defer f.mu.Unlock()
	var inv sandbox.Inventory
	for _, id := range f.order {
		inv.Pods = append(inv.Pods, sandbox.PodInfo{ID: id, Label: string(id), State: f.state[id]})
	}
	return inv
}

func (f *fakeSandbox) Pod(id sandbox.PodID) (sandbox.PodInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.state[id]
	return sandbox.PodInfo{ID: id, Label: string(id), State: st}, ok
}

// newFakeSandboxHarness wires the fake Multica server and a manager on
// a fake sandbox service.
func newFakeSandboxHarness(t *testing.T, opts ...func(*manager.Config)) (*harness, *fakeSandbox) {
	t.Helper()
	up := newFakeMultica(t)
	cfg := manager.Config{MulticaURL: up.URL, PAT: harnessPAT, Name: "Test Pool", Runtimes: []policy.Kind{policy.KindClean}}
	for _, opt := range opts {
		opt(&cfg)
	}
	logs, audit := &syncBuffer{}, &syncBuffer{}
	cfg.AuditLog = audit
	svc := newFakeSandbox()
	pool, err := manager.NewWithSandbox(cfg, slog.New(slog.NewTextHandler(logs, nil)), svc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, upstream: up, pool: pool, logs: logs, audit: audit}
	h.podAPI = httptest.NewServer(pool.PodHandler())
	h.admin = httptest.NewServer(pool.AdminHandler())
	t.Cleanup(h.podAPI.Close)
	t.Cleanup(h.admin.Close)
	return h, svc
}

// fakePod is a pod of the fake service whose daemon has not started yet.
func (h *harness) fakePod(svc *fakeSandbox, id string) *simPod {
	return &simPod{h: h, name: id, id: id, token: svc.add(id), client: h.podAPI.Client()}
}

// issueTask queues a task of an issue.
func issueTask(h *harness, id, issue string) { h.upstream.enqueueIssue(id, issue) }

func wantCalls(t *testing.T, svc *fakeSandbox, prefix string, want ...string) {
	t.Helper()
	if got := svc.seen(prefix); !slices.Equal(got, want) {
		t.Fatalf("%s calls: got %q, want %q", prefix, got, want)
	}
}

func TestAClaimIsMadeOnlyAgainstAnAcquiredPod(t *testing.T) {
	t.Parallel()
	h, svc := newFakeSandboxHarness(t)
	pod := h.fakePod(svc, "p1")
	pod.startRaw()
	svc.set("p1", sandbox.PodStarting)
	h.upstream.enqueue("t1")

	// No free pod: nothing is claimed, and the task stays queued upstream.
	h.dispatch()
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("claimed without a pod: %d claims", n)
	}
	if h.upstream.queued() != 1 {
		t.Fatal("the task left the upstream queue")
	}
	wantCalls(t, svc, "create", "create: no capacity")

	svc.set("p1", sandbox.PodFree)
	h.dispatch()
	wantCalls(t, svc, "create", "create: no capacity", "create p1")
	if n := h.upstream.claimCount(); n != 1 {
		t.Fatalf("want one claim after the pod was acquired, got %d", n)
	}
	if got := pod.pollBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 on the acquired pod, got %v", got)
	}
}

func TestAnEmptyClaimKeepsTheAcquiredPod(t *testing.T) {
	t.Parallel()
	h, svc := newFakeSandboxHarness(t)
	h.fakePod(svc, "p1").startRaw()

	h.dispatch()
	h.dispatch()
	if n := h.upstream.claimCount(); n != 2 {
		t.Fatalf("want a probe claim per tick, got %d", n)
	}
	wantCalls(t, svc, "create", "create p1")
	wantCalls(t, svc, "delete")
	if inv := h.inventory(); inv.Warm != 1 {
		t.Fatalf("the kept pod must be warm: %+v", inv)
	}
}

func TestAPollWithNothingPlacedCausesNoClaim(t *testing.T) {
	t.Parallel()
	h, svc := newFakeSandboxHarness(t)
	pod := h.fakePod(svc, "p1")
	pod.startRaw()
	h.upstream.enqueue("t1")
	for range 3 {
		if got := pod.pollBatch(); len(got) != 0 {
			t.Fatalf("a poll was handed %v", got)
		}
	}
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("a poll claimed upstream: %d", n)
	}
	wantCalls(t, svc, "create")
}

func TestAPodIsNotLentToAWorkspaceItsDaemonDidNotRegisterFor(t *testing.T) {
	t.Parallel()
	h, svc := newFakeSandboxHarness(t)
	pod := h.fakePod(svc, "p1")
	// The fake Multica lists the workspace's runtime only once some daemon
	// registered it; a second pod's daemon does that here.
	h.fakePod(svc, "p0-other").startRaw()
	svc.set("p0-other", sandbox.PodLost)
	h.upstream.enqueue("t1")

	h.dispatch()
	wantCalls(t, svc, "create", "create p1", "create: no capacity")
	if n := h.upstream.claimCount(); n != 0 {
		t.Fatalf("claimed for a pod whose daemon serves no workspace: %d", n)
	}

	pod.startRaw()
	h.dispatch()
	if got := pod.pollBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("want t1 once the daemon registered, got %v", got)
	}
	wantCalls(t, svc, "create", "create p1", "create: no capacity")
}

// personTask queues a task a person started on an issue.
func personTask(h *harness, id, issue, person string) {
	h.upstream.mu.Lock()
	defer h.upstream.mu.Unlock()
	h.upstream.queue = append(h.upstream.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": issue, "attribution": originated(person),
	})
}

// leaseFake places task id on pod by a dispatch tick and has the pod take it.
func leaseFake(t *testing.T, h *harness, pod *simPod, id string) {
	t.Helper()
	h.dispatch()
	if got := pod.pollBatch(); !slices.Equal(got, []string{id}) {
		t.Fatalf("want %s on %s, got %v", id, pod.id, got)
	}
}

func TestEveryHoldEndHandsThePodBackOnce(t *testing.T) {
	t.Parallel()
	hold := func(c *manager.Config) { c.HoldTimeout = 10 * time.Minute }
	t.Run("hold expiry", func(t *testing.T) {
		t.Parallel()
		h, svc := newFakeSandboxHarness(t, hold)
		clk := h.withClock()
		pod := h.fakePod(svc, "p1")
		pod.startRaw()
		issueTask(h, "t1", "issue-1")
		leaseFake(t, h, pod, "t1")
		pod.report("t1", "complete")
		wantCalls(t, svc, "delete")
		clk.advance(10*time.Minute + time.Second)
		h.dispatch()
		h.dispatch()
		wantCalls(t, svc, "delete", "delete p1 (hold expired)")
	})
	t.Run("no key", func(t *testing.T) {
		t.Parallel()
		h, svc := newFakeSandboxHarness(t, hold)
		pod := h.fakePod(svc, "p1")
		pod.startRaw()
		h.upstream.enqueueNoInitiator("t1")
		leaseFake(t, h, pod, "t1")
		pod.report("t1", "complete")
		h.dispatch()
		wantCalls(t, svc, "delete", "delete p1 (task ended: pod reported complete)")
	})
	t.Run("workspace removal", func(t *testing.T) {
		t.Parallel()
		h, svc := newFakeSandboxHarness(t, hold)
		pod := h.fakePod(svc, "p1")
		pod.startRaw()
		issueTask(h, "t1", "issue-1")
		leaseFake(t, h, pod, "t1")
		pod.report("t1", "complete")
		other := manager.WorkspaceConfig{WorkspaceID: "22222222-2222-4333-8444-555555555555"}
		if _, err := h.pool.Reload(context.Background(), []manager.WorkspaceConfig{other}); err != nil {
			t.Fatal(err)
		}
		h.dispatch()
		wantCalls(t, svc, "delete", "delete p1 (workspace removed)")
	})
	t.Run("budget", func(t *testing.T) {
		t.Parallel()
		h, svc := newFakeSandboxHarness(t, hold)
		pod := h.fakePod(svc, "p1")
		pod.startRaw()
		issueTask(h, "t1", "issue-1")
		leaseFake(t, h, pod, "t1")
		pod.report("t1", "complete")
		personTask(h, "t2", "issue-2", "u-bob") // another key
		h.dispatch()
		wantCalls(t, svc, "delete", "delete p1 (the pod budget is full)")
		h.dispatch()
		wantCalls(t, svc, "delete", "delete p1 (the pod budget is full)")
	})
}

// A pod deleted for the budget comes back ready before anything is claimed
// for the task that needed it.
func TestNothingIsClaimedUntilThePodDeletedForTheBudgetIsReady(t *testing.T) {
	t.Parallel()
	h, svc := newFakeSandboxHarness(t, func(c *manager.Config) { c.HoldTimeout = time.Hour })
	pod := h.fakePod(svc, "p1")
	pod.startRaw()
	issueTask(h, "t1", "issue-1")
	leaseFake(t, h, pod, "t1")
	pod.report("t1", "complete")
	claims := h.upstream.claimCount()
	personTask(h, "t2", "issue-2", "u-bob")
	h.dispatch() // p1, held for issue-1, is handed back
	h.dispatch()
	if n := h.upstream.claimCount(); n != claims {
		t.Fatalf("a claim was made before a pod was ready: %d -> %d", claims, n)
	}
	wantCalls(t, svc, "delete", "delete p1 (the pod budget is full)")

	svc.set("p1", sandbox.PodFree) // the pod in its place is ready
	h.dispatch()
	if got := pod.pollBatch(); !slices.Equal(got, []string{"t2"}) {
		t.Fatalf("want t2 on the pod once it is ready, got %v", got)
	}
}

func TestALostPodFailsItsStartedTasksAndReplacesTheOthers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		tell func(n sandbox.Notifier, id sandbox.PodID)
		fail string
	}{
		{"lost", func(n sandbox.Notifier, id sandbox.PodID) { n.Lost(id, "pod lost") }, "the pod running this task was lost"},
		{"out of memory", func(n sandbox.Notifier, id sandbox.PodID) { n.Lost(id, "the pod ran out of memory (limit 1Gi)") }, "the pod running this task was lost: the pod ran out of memory (limit 1Gi)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, svc := newFakeSandboxHarness(t, holding(time.Hour))
			a, b, other := h.fakePod(svc, "pa"), h.fakePod(svc, "pb"), h.fakePod(svc, "pc")
			a.startRaw()
			b.startRaw()
			issueTask(h, "t1", "issue-1")
			leaseFake(t, h, a, "t1")
			issueTask(h, "t2", "issue-1")
			h.dispatch() // t2 is placed on pb, not picked up yet
			other.startRaw()
			h.dispatch() // pc is acquired

			svc.set("pa", sandbox.PodLost)
			c.tell(h.pool.Notifier(), "pa")
			svc.set("pb", sandbox.PodLost)
			c.tell(h.pool.Notifier(), "pb")

			eventually(t, "t1 failed upstream", func() bool {
				msg, ok := h.upstream.failureOf("t1")
				return ok && msg == c.fail
			})
			if _, ok := h.upstream.failureOf("t2"); ok {
				t.Fatal("the unstarted task was failed")
			}
			if got := other.pollBatch(); !slices.Equal(got, []string{"t2"}) {
				t.Fatalf("want t2 placed on pc, got %v", got)
			}
			wantCalls(t, svc, "delete")
		})
	}
}

// A notification that arrives while Create is still returning names a pod the
// dispatcher does not hold yet; the pod must not be recorded as held.
func TestANotificationDuringAcquireTakesEffectAtOnce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		state sandbox.PodState
		tell  func(n sandbox.Notifier, id sandbox.PodID)
	}{
		{"lost", sandbox.PodLost, func(n sandbox.Notifier, id sandbox.PodID) { n.Lost(id, "pod lost") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, svc := newFakeSandboxHarness(t)
			h.fakePod(svc, "p1").startRaw()
			var once sync.Once
			svc.afterCreate = func(id sandbox.PodID) {
				once.Do(func() {
					svc.set(string(id), c.state)
					c.tell(h.pool.Notifier(), id)
				})
			}
			h.upstream.enqueue("t1")

			h.dispatch()
			if n := h.upstream.claimCount(); n != 0 {
				t.Fatalf("a claim was made against a pod the service no longer lent: %d claims", n)
			}
			if inv := h.inventory(); inv.Warm != 0 || inv.Running != 0 {
				t.Fatalf("the gone pod is held: %+v", inv)
			}
		})
	}
}
