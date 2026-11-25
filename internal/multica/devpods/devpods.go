// Package devpods is a simulated pod side for development: it stands in for
// the sandbox service and the stock daemon in each pod, so the manager's core
// and task source run against a real Multica server without any pod.
//
// Each simulated pod registers for every served workspace as a daemon would
// (which registers the workspace's runtimes upstream through the task
// source), picks up the tasks the core places on it, reports them started
// to Multica, and after a while completed (or, at the configured rate,
// failed). A pod handed back is deleted, and a new pod takes its place after
// a simulated start. Nothing runs: no agent, no repository, no files.
package devpods

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica"
)

// Config configures the simulated pool.
type Config struct {
	Log *slog.Logger
	Now func() time.Time
	// Pods is the number of simulated pods.
	Pods int
	// TaskMin and TaskMax bound how long a simulated task runs; each task
	// draws a duration between them.
	TaskMin, TaskMax time.Duration
	// FailRate is the share of tasks reported failed instead of completed,
	// between 0 and 1.
	FailRate float64
	// StartTime is how long the pod that replaces a deleted one takes to
	// start.
	StartTime time.Duration
	// Poll is how often each held pod looks for a task placed on it, besides
	// being woken. Zero means two seconds.
	Poll time.Duration
}

// Core is what the simulated pods ask and tell the core.
type Core interface {
	Next(ctx context.Context, pod string) json.RawMessage
	Owns(pod, task string) bool
	TaskEnded(pod, task, reason string)
	PodServes(pod, tenant string)
}

// Source is what the simulated pods ask the task source directly.
type Source interface {
	Tenants() []multica.WorkspaceRef
	Register(ctx context.Context, pod string, req multica.Registration) (map[string]json.RawMessage, string, *multica.PodError)
}

// Pool is the simulated pod side.
type Pool struct {
	cfg    Config
	log    *slog.Logger
	core   Core
	source Source
	conn   *multica.Conn
	wake   chan string
	// changed asks the registration loop to look at the workspaces now.
	changed chan struct{}

	mu   sync.Mutex
	pods []*pod
	// created numbers the pods created, for their names.
	created int
}

type pod struct {
	id         string
	state      core.PodState
	registered map[string]bool
	since      time.Time
	freeAt     time.Time
	running    map[string]bool
}

// New returns the simulated pool, its pods free and registered for nothing.
func New(cfg Config, c Core, s Source, client *multica.Client) *Pool {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.TaskMax < cfg.TaskMin {
		cfg.TaskMax = cfg.TaskMin
	}
	p := &Pool{cfg: cfg, log: cfg.Log, core: c, source: s, conn: client.Connect(),
		wake: make(chan string, 64), changed: make(chan struct{}, 1)}
	for range cfg.Pods {
		p.pods = append(p.pods, p.newPodLocked(core.PodFree))
	}
	return p
}

// newPodLocked is a new simulated pod, registered for nothing. Callers hold
// p.mu, or own p alone.
func (p *Pool) newPodLocked(state core.PodState) *pod {
	now := p.cfg.Now()
	p.created++
	return &pod{id: fmt.Sprintf("sim-%02d", p.created-1), state: state, registered: map[string]bool{}, since: now,
		freeAt: now.Add(p.cfg.StartTime), running: map[string]bool{}}
}

func (p *Pool) find(id string) *pod {
	for _, pd := range p.pods {
		if pd.id == id {
			return pd
		}
	}
	return nil
}

// settleLocked makes the pods whose start is over ready. Callers hold p.mu.
func (p *Pool) settleLocked() {
	now := p.cfg.Now()
	for _, pd := range p.pods {
		if pd.state == core.PodStarting && !now.Before(pd.freeAt) {
			pd.state = core.PodFree
		}
	}
}

// What the core asks (core.Pool).

// Acquire takes a free simulated pod.
func (p *Pool) Acquire(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settleLocked()
	for _, pd := range p.pods {
		if pd.state == core.PodFree {
			pd.state = core.PodHeld
			return pd.id, nil
		}
	}
	return "", core.ErrNoCapacity
}

// HandBack deletes the simulated pod and starts a new one in its place.
func (p *Pool) HandBack(_ context.Context, id string, why core.HandBack) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := slices.IndexFunc(p.pods, func(pd *pod) bool { return pd.id == id && pd.state == core.PodHeld })
	if i < 0 {
		return nil
	}
	p.pods[i] = p.newPodLocked(core.PodStarting)
	p.log.Info("simulated pod deleted; a new one starts", "pod_id", id, "new_pod_id", p.pods[i].id, "reason", why.Reason)
	return nil
}

func (p *Pool) info(pd *pod) core.PodInfo {
	return core.PodInfo{ID: pd.id, Name: pd.id, State: pd.state, CreatedAt: pd.since, LastSeen: p.cfg.Now()}
}

// Pod reports one simulated pod.
func (p *Pool) Pod(id string) (core.PodInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settleLocked()
	pd := p.find(id)
	if pd == nil {
		return core.PodInfo{}, false
	}
	return p.info(pd), true
}

// Pods lists the simulated pods.
func (p *Pool) Pods() []core.PodInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settleLocked()
	out := make([]core.PodInfo, 0, len(p.pods))
	for _, pd := range p.pods {
		out = append(out, p.info(pd))
	}
	return out
}

// Tenants lists the workspaces the pod registered for.
func (p *Pool) Tenants(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.find(id)
	if pd == nil {
		return nil
	}
	var out []string
	for ws := range pd.registered {
		out = append(out, ws)
	}
	slices.Sort(out)
	return out
}

// Wake tells the pod a task is placed on it.
func (p *Pool) Wake(id string) {
	select {
	case p.wake <- id:
	default:
	}
}

// Preferred: every simulated pod picks its task up at once.
func (p *Pool) Preferred(string) bool { return true }

// Record: simulated pods keep nothing across a restart.
func (p *Pool) Record(string, core.PodRecord) {}

// Halted: a simulated pool never stops claims.
func (p *Pool) Halted() string { return "" }

// What the source tells (tasksource.Pods).

// NotifyWorkspacesChanged makes the pods register for the workspaces now.
func (p *Pool) NotifyWorkspacesChanged(bool) {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

// NoteModelRequest is ignored: no simulated pod has an agent CLI to ask. The
// task source answers the picker from the models it was given instead.
func (p *Pool) NoteModelRequest(runtimeID, id string) {
	p.log.Debug("model list request ignored by the simulated pool", "runtime_id", runtimeID, "request_id", id)
}

// HasLivePods reports whether the pool has any pod.
func (p *Pool) HasLivePods() bool { return len(p.pods) > 0 }

// Handler serves nothing: no pod calls the manager.
func (p *Pool) Handler() http.Handler { return http.NotFoundHandler() }

// PrunedTotal is always zero: simulated pods are never lost.
func (p *Pool) PrunedTotal() int { return 0 }

// Health: simulated pods always start.
func (p *Pool) Health() core.PodHealth { return core.PodHealth{} }

// Run registers the pods for the served workspaces and runs the tasks placed
// on them until ctx ends.
func (p *Pool) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Poll)
	defer t.Stop()
	p.register(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.changed:
			p.register(ctx)
		case id := <-p.wake:
			p.pick(ctx, id)
		case <-t.C:
			p.register(ctx)
			for _, id := range p.held() {
				p.pick(ctx, id)
			}
		}
	}
}

func (p *Pool) held() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, pd := range p.pods {
		if pd.state == core.PodHeld {
			out = append(out, pd.id)
		}
	}
	return out
}

// register registers every pod for every served workspace it is not
// registered for yet, as the stock daemon does on start and on a
// workspaces_changed hint.
func (p *Pool) register(ctx context.Context) {
	for _, ws := range p.source.Tenants() {
		p.mu.Lock()
		var todo []string
		for _, pd := range p.pods {
			if !pd.registered[ws.ID] {
				todo = append(todo, pd.id)
			}
		}
		p.mu.Unlock()
		for _, id := range todo {
			raw, _ := json.Marshal(map[string]any{
				"workspace_id": ws.ID, "daemon_id": "devpods-" + id, "device_name": id, "cli_version": "devpods",
				"runtimes": []map[string]any{{"name": "claude", "type": "claude", "version": "0.0.0-devpods", "status": "online"}},
			})
			_, tenant, perr := p.source.Register(ctx, id, multica.Registration{
				Raw: raw, WorkspaceID: ws.ID, DeviceName: id, Offered: map[string]bool{"claude": true},
			})
			if perr != nil {
				p.log.Warn("simulated pod could not register", "pod_id", id, "workspace_id", ws.ID, "error", perr.Msg)
				break // the next pass tries again
			}
			p.mu.Lock()
			if pd := p.find(id); pd != nil {
				pd.registered[tenant] = true
			}
			p.mu.Unlock()
			p.core.PodServes(id, tenant)
		}
	}
}

// claimed is what the simulated daemon reads of a claimed task.
type claimed struct {
	ID                  string `json:"id"`
	RuntimeID           string `json:"runtime_id"`
	DispatchedAt        string `json:"dispatched_at"`
	StartClaimSupported bool   `json:"start_claim_supported"`
}

// pick takes every task placed on the pod and runs each.
func (p *Pool) pick(ctx context.Context, id string) {
	for {
		raw := p.core.Next(ctx, id)
		if raw == nil {
			return
		}
		var t claimed
		if err := json.Unmarshal(raw, &t); err != nil || t.ID == "" {
			p.log.Error("simulated pod got a task it cannot read", "pod_id", id)
			continue
		}
		p.mu.Lock()
		if pd := p.find(id); pd != nil {
			pd.running[t.ID] = true
		}
		p.mu.Unlock()
		go p.run(ctx, id, t)
	}
}

// run reports a task started, waits, and reports it completed or failed, as
// the daemon would, as long as the task is still the pod's.
func (p *Pool) run(ctx context.Context, podID string, t claimed) {
	defer func() {
		p.mu.Lock()
		if pd := p.find(podID); pd != nil {
			delete(pd.running, t.ID)
		}
		p.mu.Unlock()
	}()
	start := map[string]any{"capabilities": []string{}}
	if t.StartClaimSupported {
		start["runtime_id"], start["dispatched_at"] = t.RuntimeID, t.DispatchedAt
	}
	if err := p.conn.Post(ctx, "/api/daemon/tasks/"+t.ID+"/start", start, nil); err != nil {
		p.log.Warn("simulated task start refused", "pod_id", podID, "task_id", t.ID, "error", err)
		p.end(podID, t.ID, "fail", map[string]any{"error": "devpods: start refused: " + err.Error()})
		return
	}
	d := p.cfg.TaskMin
	if span := p.cfg.TaskMax - p.cfg.TaskMin; span > 0 {
		d += time.Duration(randInt64N(int64(span)))
	}
	p.log.Info("simulated task running", "pod_id", podID, "task_id", t.ID, "for", d.Round(time.Second))
	select {
	case <-ctx.Done():
		return
	case <-time.After(d):
	}
	if !p.core.Owns(podID, t.ID) {
		return // the core ended the lease meanwhile
	}
	if randFloat64() < p.cfg.FailRate {
		p.end(podID, t.ID, "fail", map[string]any{"error": "devpods: simulated failure"})
		return
	}
	p.end(podID, t.ID, "complete", map[string]any{"output": fmt.Sprintf("devpods: simulated run of %s on %s", d.Round(time.Second), podID)})
}

// end reports a task's end upstream and tells the core once Multica took it
// or no longer knows the task.
func (p *Pool) end(podID, taskID, action string, body map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := p.conn.Post(ctx, "/api/daemon/tasks/"+taskID+"/"+action, body, nil)
	switch status := multica.StatusOf(err); {
	case err == nil:
		p.core.TaskEnded(podID, taskID, "pod reported "+action)
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		p.core.TaskEnded(podID, taskID, fmt.Sprintf("upstream refused the pod's %s report (HTTP %d)", action, status))
	default:
		p.log.Warn("simulated task report failed; the lease is left to reconciliation", "pod_id", podID, "task_id", taskID, "action", action, "error", err)
	}
}

// randInt64N is a uniform number in [0, n), n > 0, from crypto/rand.
func randInt64N(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// randFloat64 is a uniform number in [0, 1).
func randFloat64() float64 {
	return float64(randInt64N(1<<53)) / (1 << 53)
}
