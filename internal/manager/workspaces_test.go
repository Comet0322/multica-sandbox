package manager_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/manager"

	"k8s.io/client-go/kubernetes/fake"
)

const (
	wsA = "aaaaaaaa-2222-4333-8444-555555555555"
	wsB = "bbbbbbbb-2222-4333-8444-555555555555"
)

// fakeWorkspace is one workspace's side of the fake Multica server.
type fakeWorkspace struct {
	id, runtimeID string
	// slug is what the member listing (/api/workspaces) reports; the daemon
	// listing has none, as on v0.11.0. Empty means "ws-" and the ID's first
	// eight characters.
	slug   string
	queue  []map[string]any
	claims int
	// failStatus, when non-zero, makes every claim answer with that status.
	failStatus int
	// reports records each forwarded task report as "<task id>/<action>".
	reports []string
	// calls records every request as "<method> <path>".
	calls []string
}

// multiMultica is a fake Multica server where the bot account (patPool)
// belongs to several workspaces. A request is told apart by the workspace it
// names: its X-Workspace-ID header, the workspace in its path or body, a
// runtime of the workspace, or a task the workspace handed out.
type multiMultica struct {
	*httptest.Server
	mu    sync.Mutex
	order []*fakeWorkspace
	// tasks maps a task ID to the workspace it was enqueued in.
	tasks map[string]*fakeWorkspace
}

func (ws *fakeWorkspace) slugOrDefault() string {
	if ws.slug != "" {
		return ws.slug
	}
	return "ws-" + ws.id[:8]
}

func newMultiMultica(t *testing.T, wss ...*fakeWorkspace) *multiMultica {
	t.Helper()
	m := &multiMultica{order: wss, tasks: map[string]*fakeWorkspace{}}
	for _, ws := range wss {
		ws.runtimeID = "rt-" + ws.id[:8]
	}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *multiMultica) byID(id string) *fakeWorkspace {
	for _, ws := range m.order {
		if ws.id == id {
			return ws
		}
	}
	return nil
}

func (m *multiMultica) byRuntime(id string) *fakeWorkspace {
	for _, ws := range m.order {
		if ws.runtimeID == id {
			return ws
		}
	}
	return nil
}

// workspaceOf finds the workspace a request is about; body is its decoded
// JSON body.
func (m *multiMultica) workspaceOf(r *http.Request, body map[string]any) *fakeWorkspace {
	if id := r.Header.Get("X-Workspace-ID"); id != "" {
		return m.byID(id)
	}
	if id, ok := body["workspace_id"].(string); ok {
		return m.byID(id)
	}
	if id, ok := body["runtime_id"].(string); ok {
		return m.byRuntime(id)
	}
	if ids, ok := body["runtime_ids"].([]any); ok && len(ids) > 0 {
		id, _ := ids[0].(string)
		return m.byRuntime(id)
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[1] == "workspaces":
		return m.byID(parts[2])
	case len(parts) >= 4 && parts[1] == "daemon" && parts[2] == "workspaces":
		return m.byID(parts[3])
	case len(parts) >= 4 && parts[1] == "daemon" && parts[2] == "runtimes":
		return m.byRuntime(parts[3])
	case len(parts) >= 4 && parts[1] == "daemon" && parts[2] == "tasks":
		return m.tasks[parts[3]]
	}
	return nil
}

func (m *multiMultica) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+patPool {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	// Account-level calls.
	switch {
	case r.URL.Path == "/api/me":
		writeJSON(w, map[string]any{"id": "11111111-2222-4333-8444-0000000000b1", "name": "Pool bot"})
		return
	case r.URL.Path == "/api/daemon/workspaces" || r.URL.Path == "/api/workspaces":
		list := []map[string]any{}
		for _, ws := range m.order {
			entry := map[string]any{"id": ws.id, "name": "ws " + ws.id[:4]}
			if r.URL.Path == "/api/workspaces" {
				entry["slug"] = ws.slugOrDefault()
			}
			list = append(list, entry)
		}
		writeJSON(w, list)
		return
	}
	ws := m.workspaceOf(r, body)
	if ws == nil {
		http.NotFound(w, r)
		return
	}
	ws.calls = append(ws.calls, r.Method+" "+r.URL.Path)
	switch {
	case strings.HasSuffix(r.URL.Path, "/repos"):
		writeJSON(w, []any{})
	case r.URL.Path == "/api/runtimes" && r.Method == http.MethodGet:
		writeJSON(w, []map[string]any{{"id": ws.runtimeID, "visibility": "public"}})
	case strings.HasPrefix(r.URL.Path, "/api/runtimes/") && r.Method == http.MethodPatch:
		writeJSON(w, map[string]any{"ok": true})
	case r.URL.Path == "/api/daemon/register":
		var reg struct {
			Runtimes []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"runtimes"`
		}
		_ = json.Unmarshal(raw, &reg)
		var runtimes []map[string]any
		for _, rt := range reg.Runtimes {
			runtimes = append(runtimes, map[string]any{"id": ws.runtimeID, "provider": rt.Type, "name": rt.Name})
		}
		writeJSON(w, map[string]any{"runtimes": runtimes})
	case r.URL.Path == "/api/daemon/heartbeat":
		writeJSON(w, map[string]any{"status": "ok"})
	case r.URL.Path == "/api/daemon/runtimes/"+ws.runtimeID+"/tasks/pending":
		pending := []map[string]any{}
		for _, task := range ws.queue {
			pending = append(pending, map[string]any{
				"id": task["id"], "agent_id": task["agent_id"], "runtime_id": task["runtime_id"], "issue_id": task["issue_id"],
				"status": "queued", "priority": task["priority"], "created_at": task["created_at"],
			})
		}
		writeJSON(w, pending)
	case r.URL.Path == "/api/daemon/tasks/claim":
		ws.claims++
		if ws.failStatus != 0 {
			http.Error(w, "boom", ws.failStatus)
			return
		}
		tasks := []map[string]any{}
		if len(ws.queue) > 0 {
			tasks = append(tasks, ws.queue[0])
			ws.queue = ws.queue[1:]
		}
		writeJSON(w, map[string]any{"tasks": tasks})
	case strings.HasPrefix(r.URL.Path, "/api/daemon/tasks/"):
		ws.reports = append(ws.reports, strings.TrimPrefix(r.URL.Path, "/api/daemon/tasks/"))
		writeJSON(w, map[string]any{"status": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (m *multiMultica) enqueue(ws *fakeWorkspace, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[id] = ws
	ws.queue = append(ws.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": ws.runtimeID,
		"workspace_id": ws.id, "issue_id": "issue-" + id, "initiator_email": "alice@example.com",
	})
}

// enqueueRanked adds a task with a priority and creation time; the workspace
// lists and hands out its tasks by priority descending, then oldest first, as
// the server does.
func (m *multiMultica) enqueueRanked(ws *fakeWorkspace, id string, priority int, createdAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[id] = ws
	task := map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": ws.runtimeID, "workspace_id": ws.id, "issue_id": "issue-" + id,
		"priority": priority, "created_at": createdAt.UTC().Format(time.RFC3339Nano), "initiator_email": "alice@example.com",
	}
	at := len(ws.queue)
	for i, q := range ws.queue {
		p, _ := q["priority"].(int)
		c, _ := q["created_at"].(string)
		if priority > p || (priority == p && task["created_at"].(string) < c) {
			at = i
			break
		}
	}
	ws.queue = slices.Insert(ws.queue, at, task)
}

// pendingReads counts the pending-list reads the workspace received.
func (m *multiMultica) pendingReads(ws *fakeWorkspace) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range ws.calls {
		if strings.HasSuffix(c, "/tasks/pending") {
			n++
		}
	}
	return n
}

func (m *multiMultica) claimCount(ws *fakeWorkspace) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ws.claims
}

func (m *multiMultica) setFail(ws *fakeWorkspace, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws.failStatus = status
}

func (m *multiMultica) reportsOf(ws *fakeWorkspace) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(ws.reports)
}

func newMultiHarness(t *testing.T, up *multiMultica, workspaces []manager.WorkspaceConfig) *harness {
	t.Helper()
	return newBotHarness(t, up, workspaces, manager.Config{UpstreamBackoff: time.Minute})
}

// newBotHarness runs a pool under the bot account and serves workspaces, each
// with its own settings, as a Sync that found them would.
func newBotHarness(t *testing.T, up *multiMultica, workspaces []manager.WorkspaceConfig, cfg manager.Config) *harness {
	t.Helper()
	cfg.MulticaURL, cfg.PAT, cfg.Pool.Name = up.URL, patPool, "Shared Pool"
	if len(cfg.Runtimes) == 0 {
		clean(&cfg)
	}
	return startManager(t, fake.NewSimpleClientset(), cfg, botPods, botPods, func(ctx context.Context, m *manager.Manager) error {
		_, err := m.Reload(ctx, workspaces)
		return err
	})
}

// botPods is how many warm pods a bot harness keeps, and its budget.
const botPods = 4

// registerFor registers the pod's daemon for one workspace and returns the
// runtime IDs it was given.
func (p *simPod) registerFor(workspaceID string) []string {
	p.h.t.Helper()
	body := map[string]any{
		"workspace_id": workspaceID, "daemon_id": "pod-" + p.name, "device_name": p.name,
		"cli_version": "0.0.0-test",
		"runtimes":    []map[string]any{{"name": "claude", "type": "claude", "version": "1.0.0", "status": "online"}},
	}
	status, raw := p.call(http.MethodPost, "/api/daemon/register", body)
	if status != http.StatusOK {
		p.h.t.Fatalf("register for %s: status %d: %s", workspaceID, status, raw)
	}
	var resp struct {
		Runtimes []struct {
			ID string `json:"id"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	var ids []string
	for _, rt := range resp.Runtimes {
		ids = append(ids, rt.ID)
	}
	return ids
}

// startAll plays a daemon that watches every workspace the manager lists.
func (p *simPod) startAll() []string {
	p.h.t.Helper()
	return p.startAllRaw()
}

func (p *simPod) startAllRaw() []string {
	p.h.t.Helper()
	status, raw := p.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK {
		p.h.t.Fatalf("list workspaces: status %d", status)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		p.h.t.Fatal(err)
	}
	var ids []string
	for _, ws := range list {
		ids = append(ids, p.registerFor(ws.ID)...)
	}
	return ids
}

// claimFrom plays a dispatch tick and then the daemon's claim (see
// claimBatch).
func (p *simPod) claimFrom(runtimeIDs []string) []string {
	p.h.t.Helper()
	p.h.dispatch()
	status, raw := p.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
		"daemon_id": "pod-" + p.name, "runtime_ids": runtimeIDs, "max_tasks": 4,
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("claim: status %d: %s", status, raw)
	}
	var resp struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	ids := []string{}
	for _, task := range resp.Tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func twoWorkspaces(a, b func(*manager.WorkspaceConfig)) []manager.WorkspaceConfig {
	ca := manager.WorkspaceConfig{WorkspaceID: wsA, RuntimeName: "Pool A"}
	cb := manager.WorkspaceConfig{WorkspaceID: wsB, RuntimeName: "Pool B"}
	if a != nil {
		a(&ca)
	}
	if b != nil {
		b(&cb)
	}
	return []manager.WorkspaceConfig{ca, cb}
}

func TestPodSeesEveryWorkspaceAndTasksAreClaimedInTheirOwnWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	up.enqueue(b, "tb")
	pod := h.newPod("pool-0")

	status, raw := pod.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), wsA) || !strings.Contains(string(raw), wsB) {
		t.Fatalf("pod must see both workspaces, got %d %s", status, raw)
	}
	ids := pod.startAll()
	if len(ids) != 2 {
		t.Fatalf("want a runtime per workspace, got %v", ids)
	}
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("want [tb], got %v", got)
	}
	if status := pod.report("tb", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	// The report reached B, the workspace that handed the task out.
	if got := up.reportsOf(b); !slices.Equal(got, []string{"tb/complete"}) {
		t.Fatalf("workspace B should see the report, got %v", got)
	}
	if got := up.reportsOf(a); len(got) != 0 {
		t.Fatalf("workspace A must see nothing, got %v", got)
	}
}

func TestAPodLentToAIsNeverLentToB(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	up.enqueue(a, "ta")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"ta"}) {
		t.Fatalf("want [ta], got %v", got)
	}
	if status := pod.report("ta", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}

	// B has a task the moment A's lease ends. A's clean pod is deleted, and
	// B's queue is not touched before a new pod is ready.
	up.enqueue(b, "tb")
	before := up.claimCount(b)
	eventually(t, "A's pod deleted", func() bool { return !h.kubeHas(pod.id) })
	if code := pod.heartbeat(); code != http.StatusUnauthorized {
		t.Fatalf("the deleted pod's daemon still reaches the manager: %d", code)
	}
	h.dispatch()
	if n := up.claimCount(b) - before; n != 0 {
		t.Fatalf("B's queue must not be touched before a new pod is ready, saw %d claims", n)
	}
	ids = pod.recycleMulti()
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("want [tb] on the new pod, got %v", got)
	}
}

func TestWorkspaceUpstreamFailureDoesNotAffectOthers(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
			up := newMultiMultica(t, a, b)
			h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
			up.setFail(a, status)
			up.enqueue(b, "tb1")
			up.enqueue(b, "tb2")
			p1, p2 := h.newPod("pool-0"), h.newPod("pool-1")
			ids1, ids2 := p1.startAll(), p2.startAll()

			if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"tb1"}) {
				t.Fatalf("B must be served while A fails, got %v", got)
			}
			// A is left alone for the backoff; B keeps being served.
			failedClaims := up.claimCount(a)
			if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb2"}) {
				t.Fatalf("B must keep being served, got %v", got)
			}
			if n := up.claimCount(a); n != failedClaims {
				t.Fatalf("failing workspace was polled again within the backoff: %d -> %d", failedClaims, n)
			}
		})
	}
}

func TestWorkspaceAtItsQuotaIsNotPolled(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(func(c *manager.WorkspaceConfig) { c.Quota = 1 }, nil))
	p1, p2 := h.newPod("pool-0"), h.newPod("pool-1")
	ids1, ids2 := p1.startAll(), p2.startAll()
	up.enqueue(a, "ta1")
	up.enqueue(a, "ta2")
	up.enqueue(b, "tb1")

	if got := p1.claimFrom(ids1); !slices.Equal(got, []string{"ta1"}) {
		t.Fatalf("want [ta1], got %v", got)
	}
	aClaims := up.claimCount(a)
	if got := p2.claimFrom(ids2); !slices.Equal(got, []string{"tb1"}) {
		t.Fatalf("A is capped, so B's task goes to the free pod; got %v", got)
	}
	if n := up.claimCount(a); n != aClaims {
		t.Fatalf("capped workspace was polled: %d -> %d", aClaims, n)
	}
	if up.queuedOf(a) != 1 {
		t.Fatal("A's second task must stay queued in Multica")
	}
}

func (m *multiMultica) queuedOf(ws *fakeWorkspace) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(ws.queue)
}

func TestWorkspacesTakeTurnsWhenBothHaveTasks(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	// The global queue orders claims oldest first, across workspaces.
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"a1", "a2", "a3"} {
		up.enqueueRanked(a, id, 0, t0.Add(time.Duration(2*i)*time.Second))
	}
	for i, id := range []string{"b1", "b2", "b3"} {
		up.enqueueRanked(b, id, 0, t0.Add(time.Duration(2*i+1)*time.Second))
	}
	var got []string
	for i, ip := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4", "127.0.0.5"} {
		pod := h.newPod("pool-" + ip)
		_ = i
		got = append(got, pod.claimFrom(pod.startAll())...)
	}
	var fromA, fromB int
	for _, id := range got {
		if strings.HasPrefix(id, "a") {
			fromA++
		} else {
			fromB++
		}
	}
	if fromA != 2 || fromB != 2 {
		t.Fatalf("want 2 tasks from each workspace, got %v", got)
	}
}

func TestInventoryLeasesAndAuditCarryTheWorkspace(t *testing.T) {
	t.Parallel()
	a, b := &fakeWorkspace{id: wsA}, &fakeWorkspace{id: wsB}
	up := newMultiMultica(t, a, b)
	h := newMultiHarness(t, up, twoWorkspaces(nil, nil))
	pod := h.newPod("pool-0")
	ids := pod.startAll()
	up.enqueue(b, "tb")
	if got := pod.claimFrom(ids); !slices.Equal(got, []string{"tb"}) {
		t.Fatalf("want [tb], got %v", got)
	}

	inv := h.inventory()
	if i := slices.IndexFunc(inv.Pods, func(p manager.PodInfo) bool { return p.ID == pod.id }); i < 0 || inv.Pods[i].WorkspaceID != wsB {
		t.Fatalf("leased pod must show workspace B: %+v", inv.Pods)
	}
	counts := map[string]int{}
	for _, ws := range inv.Workspaces {
		counts[ws.WorkspaceID] = ws.Leased
	}
	if counts[wsA] != 0 || counts[wsB] != 1 {
		t.Fatalf("per-workspace lease counts wrong: %+v", inv.Workspaces)
	}
	if ls := h.leases(); len(ls) != 1 || ls[0].WorkspaceID != wsB {
		t.Fatalf("lease record must carry workspace B: %+v", ls)
	}

	pod.report("tb", "complete")
	for _, rec := range h.auditRecords() {
		if rec.TaskID == "" {
			continue // a pod-level record, tied to no workspace
		}
		if rec.WorkspaceID != wsB {
			t.Fatalf("audit record without workspace B: %+v", rec)
		}
	}
	if recs := h.auditRecords(); len(recs) < 2 {
		t.Fatalf("want lease and release records, got %+v", recs)
	}
}

func TestPoolRejectsBadWorkspaceEntries(t *testing.T) {
	t.Parallel()
	cases := map[string][]manager.WorkspaceConfig{
		"duplicate":      {{WorkspaceID: wsA}, {WorkspaceID: wsA}},
		"bad ID":         {{WorkspaceID: "nope"}},
		"negative quota": {{WorkspaceID: wsA, Quota: -1}},
	}
	up := newMultiMultica(t, &fakeWorkspace{id: wsA})
	pool, err := manager.New(manager.Config{MulticaURL: up.URL, PAT: patPool, Kube: testKube()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, ws := range cases {
		if _, err := pool.Reload(context.Background(), ws); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// callCount counts the requests the workspace received whose "<method> <path>"
// is call.
func (m *multiMultica) callCount(ws *fakeWorkspace, call string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range ws.calls {
		if c == call {
			n++
		}
	}
	return n
}
