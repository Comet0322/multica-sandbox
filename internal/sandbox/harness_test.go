package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

// fakeClock is the service's time source in tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// notice is one call of the notification hook.
type notice struct {
	kind string // "interrupted" or "lost"
	id   sandbox.PodID
	why  string
}

// recorder is the caller's notification hook and audit writer. Each hook call
// checks that the service's lock is not held by calling back into it.
type recorder struct {
	mu      sync.Mutex
	svc     *sandbox.GitOpsService
	notices []notice
	audit   []sandbox.AuditRecord
}

func (r *recorder) note(kind string, id sandbox.PodID, why string) {
	r.svc.Inventory() // deadlocks if the service called the hook with its lock held
	r.mu.Lock()
	r.notices = append(r.notices, notice{kind, id, why})
	r.mu.Unlock()
}

func (r *recorder) Interrupted(id sandbox.PodID, why string) { r.note("interrupted", id, why) }
func (r *recorder) Lost(id sandbox.PodID, why string)        { r.note("lost", id, why) }

func (r *recorder) Record(rec sandbox.AuditRecord) {
	r.mu.Lock()
	r.audit = append(r.audit, rec)
	r.mu.Unlock()
}

func (r *recorder) takeNotices() []notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.notices
	r.notices = nil
	return out
}

func (r *recorder) records() []sandbox.AuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.audit)
}

type harness struct {
	t     *testing.T
	svc   *sandbox.GitOpsService
	srv   *httptest.Server
	clock *fakeClock
	// mono is the service's stall clock. It stands still unless a test moves
	// it, as for a process that kept sweeping while clock moved on.
	mono  *fakeClock
	rec   *recorder
	logs  *bytes.Buffer
	logMu sync.Mutex
}

func newHarness(t *testing.T, opts ...func(*sandbox.Config)) *harness {
	t.Helper()
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	h := &harness{t: t, clock: &fakeClock{t: start}, mono: &fakeClock{t: start}, rec: &recorder{}, logs: &bytes.Buffer{}}
	cfg := sandbox.Config{
		Now:       h.clock.now,
		Monotonic: h.mono.now,
		Notify:    h.rec,
		Audit:     h.rec,
		Log:       slog.New(slog.NewTextHandler(lockedWriter{h}, nil)),
	}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := sandbox.NewGitOps(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.svc, h.rec.svc = svc, svc
	h.srv = httptest.NewServer(svc.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

type lockedWriter struct{ h *harness }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.h.logMu.Lock()
	defer w.h.logMu.Unlock()
	return w.h.logs.Write(p)
}

func (h *harness) logText() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.logs.String()
}

// advance moves the service's clock only: time passing for a process that
// kept sweeping all along.
func (h *harness) advance(d time.Duration) {
	h.clock.advance(d)
}

// stall moves both clocks, as for a process that did not run meanwhile.
func (h *harness) stall(d time.Duration) {
	h.clock.advance(d)
	h.mono.advance(d)
}

// states maps every listed pod to its state.
func (h *harness) states() map[sandbox.PodID]sandbox.PodState {
	out := map[sandbox.PodID]sandbox.PodState{}
	for _, p := range h.svc.Inventory().Pods {
		out[p.ID] = p.State
	}
	return out
}

func (h *harness) wantState(p *simPod, want sandbox.PodState) {
	h.t.Helper()
	if got := h.states()[p.id]; got != want {
		h.t.Fatalf("pod %s: state %q, want %q", p.id, got, want)
	}
}

func (h *harness) create() (sandbox.PodID, error) {
	return h.svc.Create(context.Background())
}

func (h *harness) mustCreate() sandbox.PodID {
	h.t.Helper()
	id, err := h.create()
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	return id
}

func (h *harness) wantNoCapacity() {
	h.t.Helper()
	if id, err := h.create(); err != sandbox.ErrNoCapacity {
		h.t.Fatalf("create: got %q, %v; want ErrNoCapacity", id, err)
	}
}

func (h *harness) delete(id sandbox.PodID) {
	h.t.Helper()
	if err := h.svc.Delete(context.Background(), id, sandbox.AuditLabel{Reason: "test"}); err != nil {
		h.t.Fatalf("delete: %v", err)
	}
}

// simPod plays a pod: its entrypoint registering it and its supervisor on the
// control channel.
type simPod struct {
	h     *harness
	id    sandbox.PodID
	token string
}

func (h *harness) do(method, path, token string, body any) (int, []byte) {
	h.t.Helper()
	var r io.Reader
	if s, ok := body.(string); ok {
		r = bytes.NewBufferString(s)
	} else if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// register plays the entrypoint registering a new pod.
func (h *harness) register(label string) *simPod {
	h.t.Helper()
	status, raw := h.do(http.MethodPost, "/pod/register", "", map[string]string{"label": label})
	if status != http.StatusOK {
		h.t.Fatalf("register: status %d: %s", status, raw)
	}
	var got struct {
		PodID string `json:"pod_id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		h.t.Fatal(err)
	}
	return &simPod{h: h, id: sandbox.PodID(got.PodID), token: got.Token}
}

type command struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

// poll plays the supervisor's poll; nil means no command.
func (p *simPod) poll() *command {
	p.h.t.Helper()
	status, raw := p.h.do(http.MethodGet, "/pod/control", p.token, nil)
	if status == http.StatusNoContent {
		return nil
	}
	if status != http.StatusOK {
		p.h.t.Fatalf("poll: status %d: %s", status, raw)
	}
	var cmd command
	if err := json.Unmarshal(raw, &cmd); err != nil {
		p.h.t.Fatal(err)
	}
	return &cmd
}

func (p *simPod) report(id string, failed ...string) int {
	p.h.t.Helper()
	status, _ := p.h.do(http.MethodPost, "/pod/control", p.token, map[string]any{"id": id, "status": "done", "failed": failed})
	return status
}

func (p *simPod) event(name string) int {
	p.h.t.Helper()
	status, _ := p.h.do(http.MethodPost, "/pod/control", p.token, map[string]string{"event": name})
	return status
}

// completeReset plays the supervisor taking its reset command and reporting
// it done.
func (p *simPod) completeReset() {
	p.h.t.Helper()
	cmd := p.poll()
	if cmd == nil || cmd.Command != "reset" || cmd.ID == "" {
		p.h.t.Fatalf("want a reset command, got %+v", cmd)
	}
	if status := p.report(cmd.ID); status != http.StatusOK {
		p.h.t.Fatalf("report done: status %d", status)
	}
}

// registerClean registers a pod and completes its first reset, so it is free.
func (h *harness) registerClean(label string) *simPod {
	h.t.Helper()
	p := h.register(label)
	p.completeReset()
	return p
}
