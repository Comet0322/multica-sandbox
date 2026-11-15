package manager_test

// The task-lifecycle harness: a fake Multica server, a fake Kubernetes API
// the manager creates its pods in, simulated daemons that present the pod
// token the manager put in their pod's environment, and the manager under
// test, all inside one test process. Every simulated pod reaches the manager
// from the same loopback address: only the token tells them apart.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"sandbox-manager/internal/manager"
)

const (
	harnessWorkspaceID = "11111111-2222-4333-8444-555555555555"
	harnessPAT         = "mul_test_manager_pat"
)

// fakeMultica stands in for the Multica server the manager talks to as the bot
// account's daemon: the account belongs to the one workspace harnessWorkspaceID.
type fakeMultica struct {
	*httptest.Server

	mu    sync.Mutex
	calls []string
	// registrations holds the bodies of /api/daemon/register calls received.
	registrations []map[string]any
	// queue holds tasks the fake hands out on claim; claims counts claim
	// requests of either flavour that reached the server.
	queue  []map[string]any
	claims int
	// taskCalls records forwarded task reports as "<task id> <action>"; failures
	// maps a failed task id to the error text reported.
	taskCalls []string
	failures  map[string]string
	// agentCalls records requests that carried a task token, as
	// "<method> <path> <Authorization header>".
	agentCalls []string
	// hook, when set, may answer a request itself; it returns false to leave
	// the request to the defaults.
	hook func(w http.ResponseWriter, r *http.Request) bool
}

// intercept installs a hook that answers some requests before the defaults.
func (f *fakeMultica) intercept(hook func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

// callCount counts the requests received whose "<method> <path>" is call.
func (f *fakeMultica) callCount(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func newFakeMultica(t *testing.T) *fakeMultica {
	t.Helper()
	f := &fakeMultica{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMultica) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer mat_") {
		// Agent traffic: the server authenticates the task token itself.
		f.mu.Lock()
		f.agentCalls = append(f.agentCalls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+harnessPAT {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	switch r.URL.Path {
	case "/api/me":
		writeJSON(w, map[string]any{"id": "11111111-2222-4333-8444-0000000000b0", "name": "Pool bot"})
	case "/api/daemon/workspaces", "/api/workspaces":
		writeJSON(w, []map[string]any{{"id": harnessWorkspaceID, "name": "Harness"}})
	case "/api/runtimes":
		// The registered runtime, already shared with the workspace.
		list := []map[string]any{}
		if f.registrationCount() > 0 {
			list = append(list, map[string]any{"id": "runtime-a", "visibility": "public"})
		}
		writeJSON(w, list)
	case "/api/runtimes/runtime-a":
		writeJSON(w, map[string]any{"ok": true})
	case "/api/daemon/runtimes/runtime-a/tasks/pending":
		f.mu.Lock()
		pending := []map[string]any{}
		for _, task := range f.queue {
			pending = append(pending, map[string]any{"id": task["id"], "agent_id": task["agent_id"], "runtime_id": task["runtime_id"], "issue_id": task["issue_id"], "status": "queued"})
		}
		f.mu.Unlock()
		writeJSON(w, pending)
	case "/api/daemon/register":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.registrations = append(f.registrations, body)
		f.mu.Unlock()
		var runtimes []map[string]any
		for i, rt := range body["runtimes"].([]any) {
			m := rt.(map[string]any)
			runtimes = append(runtimes, map[string]any{
				"id":       "runtime-" + string(rune('a'+i)),
				"provider": m["type"],
				"name":     m["name"],
			})
		}
		writeJSON(w, map[string]any{"runtimes": runtimes})
	case "/api/daemon/heartbeat":
		writeJSON(w, map[string]any{"status": "ok"})
	case "/api/daemon/runtimes/runtime-a/recover-orphans":
		writeJSON(w, map[string]any{"orphaned": 0, "retried": 0})
	case "/api/daemon/tasks/claim":
		var body struct {
			MaxTasks int `json:"max_tasks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tasks := []map[string]any{}
		for len(tasks) < body.MaxTasks {
			task := f.pop()
			if task == nil {
				break
			}
			tasks = append(tasks, task)
		}
		writeJSON(w, map[string]any{"tasks": tasks})
	case "/api/daemon/runtimes/runtime-a/tasks/claim":
		writeJSON(w, map[string]any{"task": f.pop()})
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/daemon/tasks/"), "/")
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/tasks/") || len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.taskCalls = append(f.taskCalls, parts[0]+" "+parts[1])
		if parts[1] == "fail" {
			var body struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if f.failures == nil {
				f.failures = map[string]string{}
			}
			f.failures[parts[0]] = body.Error
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"status": "ok"})
	}
}

// enqueue adds a task the next claim will return.
func (f *fakeMultica) enqueue(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": "issue-" + id,
		"initiator_email": "alice@example.com",
	})
}

// enqueueNoInitiator adds a task that no human triggered, an autopilot run
// with no issue or chat session, so no key.
func (f *fakeMultica) enqueueNoInitiator(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID,
	})
}

func (f *fakeMultica) pop() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if len(f.queue) == 0 {
		return nil
	}
	task := f.queue[0]
	f.queue = f.queue[1:]
	return task
}

func (f *fakeMultica) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeMultica) queued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

func (f *fakeMultica) taskCallsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.taskCalls...)
}

func (f *fakeMultica) failureOf(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, ok := f.failures[id]
	return msg, ok
}

func (f *fakeMultica) registrationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registrations)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// harness wires the fake server and a manager together.
type harness struct {
	t        *testing.T
	upstream *fakeMultica
	pool     *manager.Manager
	podAPI   *httptest.Server
	admin    *httptest.Server
	// logs captures the manager's log output.
	logs *syncBuffer
	// audit captures the manager's audit log.
	audit *syncBuffer
	// kube is the Kubernetes API the manager creates its pods in, and stop
	// ends the manager.
	kube *fake.Clientset
	stop context.CancelFunc
	// adopted are the pods a simulated daemon plays, by name.
	adoptMu sync.Mutex
	adopted map[string]bool
}

// syncBuffer is a log sink safe for the manager's concurrent handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newHarness starts a manager whose sandbox service keeps expectedSize warm
// pods, which is also its budget. Tests that do not choose claim for the
// pool kind with no hold.
func newHarness(t *testing.T, expectedSize int, opts ...func(*manager.Config)) *harness {
	t.Helper()
	return newKubeWorld(t).manager(expectedSize, max(expectedSize, 1), opts...)
}

// inventory queries the manager's read-only inventory endpoint.
func (h *harness) inventory() manager.Inventory {
	h.t.Helper()
	resp, err := http.Get(h.admin.URL + "/v1/inventory")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("inventory status %d", resp.StatusCode)
	}
	var inv manager.Inventory
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		h.t.Fatal(err)
	}
	return inv
}

// simPod plays a pod's stock daemon, presenting the pod token the manager
// put in the pod's environment.
type simPod struct {
	h    *harness
	name string
	// id is the pod's name in Kubernetes, token its pod token.
	id, token string
	client    *http.Client
}

// newPod plays the daemon of the next warm pod the manager created that no
// simulated daemon plays yet; name is the daemon's own name, which the
// manager never shows.
func (h *harness) newPod(name string) *simPod {
	h.t.Helper()
	var p *simPod
	eventually(h.t, "a new pod", func() bool {
		for _, pd := range h.kubePods() {
			h.adoptMu.Lock()
			taken := h.adopted[pd.Name]
			if !taken {
				h.adopted[pd.Name] = true
			}
			h.adoptMu.Unlock()
			if !taken {
				p = h.adopt(pd)
				p.name = name
				return true
			}
		}
		return false
	})
	return p
}

func (p *simPod) call(method, path string, body any) (int, []byte) {
	p.h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, p.h.podAPI.URL+path, rd)
	if err != nil {
		p.h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// register sends the daemon's register request alone and returns the raw reply.
func (p *simPod) register() (int, []byte) {
	p.h.t.Helper()
	body := map[string]any{
		"workspace_id": harnessWorkspaceID,
		"daemon_id":    "pod-" + p.name,
		"device_name":  p.name,
		"cli_version":  "0.0.0-test",
		"runtimes": []map[string]any{
			{"name": "claude", "type": "claude", "version": "1.0.0", "status": "online"},
		},
	}
	return p.call(http.MethodPost, "/api/daemon/register", body)
}

// heartbeat sends one daemon heartbeat and returns the status code.
func (p *simPod) heartbeat() int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	return status
}

// claimBatch plays a dispatch tick and then the daemon's machine-level claim,
// and returns the task IDs the pod was handed. In a running manager the tick
// runs every second, so between two polls of a daemon it has always run;
// pollBatch is the poll alone.
func (p *simPod) claimBatch() []string {
	p.h.t.Helper()
	p.h.dispatch()
	return p.pollBatch()
}

// pollBatch plays the daemon's machine-level claim alone and returns the task
// IDs it was handed.
func (p *simPod) pollBatch() []string {
	p.h.t.Helper()
	status, raw := p.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
		"daemon_id": "pod-" + p.name, "runtime_ids": []string{"runtime-a"}, "max_tasks": 4,
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("batch claim: status %d: %s", status, raw)
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

// report sends a task report (start, progress, complete, fail) and returns the
// status code.
func (p *simPod) report(taskID, action string) int {
	p.h.t.Helper()
	status, _ := p.call(http.MethodPost, "/api/daemon/tasks/"+taskID+"/"+action, map[string]any{"error": "boom"})
	return status
}

// leases queries the manager's lease records.
func (h *harness) leases() []manager.LeaseInfo {
	h.t.Helper()
	resp, err := http.Get(h.admin.URL + "/v1/leases")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("leases status %d", resp.StatusCode)
	}
	var out []manager.LeaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// start runs the calls a daemon makes while starting up: renew its token, list
// workspaces, then register its runtimes. It returns the register response.
// The pod is ready once start returns.
func (p *simPod) start() map[string]any {
	p.h.t.Helper()
	return p.startRaw()
}

// startRaw is one daemon start.
func (p *simPod) startRaw() map[string]any {
	p.h.t.Helper()
	if status, _ := p.call(http.MethodPost, "/api/tokens/current/renew", map[string]any{}); status != http.StatusOK {
		p.h.t.Fatalf("renew token: status %d", status)
	}
	status, raw := p.call(http.MethodGet, "/api/workspaces", nil)
	if status != http.StatusOK {
		p.h.t.Fatalf("list workspaces: status %d", status)
	}
	if !strings.Contains(string(raw), harnessWorkspaceID) {
		p.h.t.Fatalf("workspace list %s does not offer %s", raw, harnessWorkspaceID)
	}
	status, raw = p.register()
	if status != http.StatusOK {
		p.h.t.Fatalf("register: status %d: %s", status, raw)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	return resp
}

// enqueueTask adds a task with the given initiator and the agent's own env.
func (f *fakeMultica) enqueueTask(id, initiator string, customEnv map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task := map[string]any{
		"id": id, "agent_id": "agent-1", "runtime_id": "runtime-a",
		"workspace_id": harnessWorkspaceID, "issue_id": "issue-" + id,
		"initiator_email": initiator,
		"agent":           map[string]any{"id": "agent-1", "name": "A", "instructions": "keep", "custom_env": customEnv},
	}
	f.queue = append(f.queue, task)
}

// claimTask plays a dispatch tick and the daemon's machine-level claim, and
// returns the raw task.
func (p *simPod) claimTask() map[string]any {
	p.h.t.Helper()
	p.h.dispatch()
	status, raw := p.call(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
		"daemon_id": "pod-" + p.name, "runtime_ids": []string{"runtime-a"}, "max_tasks": 4,
	})
	if status != http.StatusOK {
		p.h.t.Fatalf("claim: status %d: %s", status, raw)
	}
	var resp struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		p.h.t.Fatal(err)
	}
	if len(resp.Tasks) == 0 {
		return nil
	}
	return resp.Tasks[0]
}

// envOf returns the agent's custom environment carried by a claimed task.
func envOf(t *testing.T, task map[string]any) map[string]string {
	t.Helper()
	agent, _ := task["agent"].(map[string]any)
	raw, _ := agent["custom_env"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		out[k], _ = v.(string)
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const runtimeGoneBody = `{"error":"runtime not found"}`

// heartbeatAnswer makes the fake answer the manager's upstream heartbeats with
// the given status and body while on is true.
func heartbeatAnswer(f *fakeMultica, on *atomic.Bool, status int, body string) {
	f.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/heartbeat" && on.Load() {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return true
		}
		return false
	})
}

// testClock is the manager's clock plus an offset the test moves.
type testClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

// withClock gives the pool a clock the test can move. The harness pools are
// quiet until the first request, so the swap cannot race anything.
func (h *harness) withClock() *testClock {
	h.t.Helper()
	c := &testClock{}
	h.pool.SetNow(c.now)
	return c
}

// leaseOne starts the pod and hands it task id.
func leaseOne(t *testing.T, h *harness, pod *simPod, id string) {
	t.Helper()
	h.upstream.enqueue(id)
	pod.start()
	if got := pod.claimBatch(); !slices.Equal(got, []string{id}) {
		t.Fatalf("want %s, got %v", id, got)
	}
}

func (h *harness) lease(id string) manager.LeaseInfo {
	h.t.Helper()
	for _, l := range h.leases() {
		if l.TaskID == id {
			return l
		}
	}
	h.t.Fatalf("no lease record for %s in %+v", id, h.leases())
	return manager.LeaseInfo{}
}

// wantEnded fails unless the lease of id has ended with a reason containing
// reason.
func (h *harness) wantEnded(id, reason string) {
	h.t.Helper()
	l := h.lease(id)
	if l.EndedAt == nil || !strings.Contains(l.EndReason, reason) {
		h.t.Fatalf("lease %s: want ended with %q, got %+v", id, reason, l)
	}
}

func (h *harness) wantHeld(id string) {
	h.t.Helper()
	if l := h.lease(id); l.EndedAt != nil {
		h.t.Fatalf("lease %s must be held, ended: %+v", id, l)
	}
}

// answerTask answers the task endpoint of the fake before its defaults: the
// answer function gets the task id and the action and returns an HTTP status
// (0 passes the request on) and, for a GET of status, the status text.
func answerTask(f *fakeMultica, answer func(id, action string) (code int, status string)) {
	f.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/daemon/tasks/")
		if !ok {
			return false
		}
		parts := strings.Split(rest, "/")
		if len(parts) != 2 {
			return false
		}
		code, status := answer(parts[0], parts[1])
		switch {
		case code != 0:
			http.Error(w, "refused", code)
		case status != "":
			writeJSON(w, map[string]any{"status": status})
		default:
			return false
		}
		return true
	})
}
