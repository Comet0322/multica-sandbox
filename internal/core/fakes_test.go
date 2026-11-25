package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/policy"
)

// fakeSource is a task source in memory: each tenant has a queue of tasks a
// claim takes from the front.
type fakeSource struct {
	mu     sync.Mutex
	queues map[string][]core.Task
	order  []string
	fails  []string // "tenant task reason"
	// failErr is what Fail returns; nil takes every report.
	failErr error
	closed  []string
	ended   map[string]string // task -> reason, for TaskEnded
	claims  []string          // tenants claimed from, in order
	pending map[string]error  // tenant -> error from Pending
	created map[string]time.Time
}

func newFakeSource(tenants ...string) *fakeSource {
	return &fakeSource{queues: map[string][]core.Task{}, order: tenants, ended: map[string]string{}, pending: map[string]error{}, created: map[string]time.Time{}}
}

// add queues a pool task of a tenant for a person, created at t0+age.
func (s *fakeSource) add(tenant, id, person string, age time.Duration) {
	s.addTask(tenant, policy.KindPool, id, "issue:"+id, person, age)
}

// addTask queues a task of a tenant's runtimes of kind, with key.
func (s *fakeSource) addTask(tenant string, kind policy.Kind, id, key, person string, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := core.Task{
		Task:    policy.Task{ID: id, Tenant: tenant, Kind: kind, Key: key},
		Email:   person + "@example.com",
		Payload: json.RawMessage(fmt.Sprintf(`{"id":%q}`, id)),
	}
	s.created[id] = t0.Add(age)
	s.queues[tenant] = append(s.queues[tenant], t)
}

func (s *fakeSource) Claimable() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}

func (s *fakeSource) Pending(_ context.Context, tenant string, kind policy.Kind) ([]policy.Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pending[tenant]; err != nil {
		return nil, err
	}
	var out []policy.Pending
	for _, t := range s.queues[tenant] {
		if t.Kind != kind {
			continue
		}
		out = append(out, policy.Pending{ID: t.ID, Priority: 1, CreatedAt: s.created[t.ID], Status: policy.StatusQueued})
	}
	return out, nil
}

func (s *fakeSource) Claim(_ context.Context, tenant string, kind policy.Kind) (*core.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims = append(s.claims, tenant)
	q := s.queues[tenant]
	i := slices.IndexFunc(q, func(t core.Task) bool { return t.Kind == kind })
	if i < 0 {
		return nil, nil
	}
	t := q[i]
	s.queues[tenant] = slices.Delete(slices.Clone(q), i, i+1)
	return &t, nil
}

func (s *fakeSource) Fail(_ context.Context, tenant, taskID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, tenant+" "+taskID+" "+reason)
	return s.failErr
}

// failWith makes Fail return err, nil to take reports again.
func (s *fakeSource) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failErr = err
}

func (s *fakeSource) TaskEnded(_ context.Context, _, taskID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason, ok := s.ended[taskID]
	return reason, ok
}

func (s *fakeSource) Close(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, tenant)
}

func (s *fakeSource) snapshot() (claims, fails, closed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.claims), slices.Clone(s.fails), slices.Clone(s.closed)
}

// fakePool is a pool in memory: pods are free until acquired, and serve the
// tenants given. A pod handed back is deleted and a new one starts in its
// place under the same ID, ready once ready is called.
type fakePool struct {
	mu      sync.Mutex
	pods    []*fakePod
	woken   []string
	backs   []string // "pod reason"
	preferd map[string]bool
	records map[string]core.PodRecord
	halted  string
}

func (p *fakePool) halt(why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.halted = why
}

func (p *fakePool) Halted() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.halted
}

type fakePod struct {
	id      string
	state   core.PodState
	tenants []string
}

func newFakePool() *fakePool {
	return &fakePool{preferd: map[string]bool{}, records: map[string]core.PodRecord{}}
}

// restore adds a pod a previous run left, held by the core.
func (p *fakePool) restore(id string, tenants ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pods = append(p.pods, &fakePod{id: id, state: core.PodHeld, tenants: tenants})
}

func (p *fakePool) Record(id string, rec core.PodRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records[id] = rec
}

func (p *fakePool) record(id string) core.PodRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.records[id]
}

// add registers a free pod serving tenants.
func (p *fakePool) add(id string, tenants ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pods = append(p.pods, &fakePod{id: id, state: core.PodFree, tenants: tenants})
}

func (p *fakePool) find(id string) *fakePod {
	for _, pd := range p.pods {
		if pd.id == id {
			return pd
		}
	}
	return nil
}

// ready makes the pod that replaced a handed-back one ready.
func (p *fakePool) ready(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pd := p.find(id); pd != nil && pd.state == core.PodStarting {
		pd.state = core.PodFree
	}
}

// lose marks a pod lost.
func (p *fakePool) lose(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.find(id).state = core.PodLost
}

func (p *fakePool) Acquire(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pd := range p.pods {
		if pd.state == core.PodFree {
			pd.state = core.PodHeld
			return pd.id, nil
		}
	}
	return "", core.ErrNoCapacity
}

func (p *fakePool) HandBack(_ context.Context, id string, why core.HandBack) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pd := p.find(id); pd != nil && pd.state == core.PodHeld {
		pd.state = core.PodStarting
	}
	p.backs = append(p.backs, id+" "+why.Reason)
	return nil
}

func (p *fakePool) Pod(id string) (core.PodInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.find(id)
	if pd == nil {
		return core.PodInfo{}, false
	}
	return core.PodInfo{ID: pd.id, Name: pd.id, State: pd.state}, true
}

func (p *fakePool) Pods() []core.PodInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []core.PodInfo
	for _, pd := range p.pods {
		out = append(out, core.PodInfo{ID: pd.id, Name: pd.id, State: pd.state})
	}
	return out
}

func (p *fakePool) Tenants(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pd := p.find(id); pd != nil {
		return slices.Sorted(slices.Values(pd.tenants))
	}
	return nil
}

func (p *fakePool) Wake(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.woken = append(p.woken, id)
}

func (p *fakePool) Preferred(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preferd[id]
}

func (p *fakePool) snapshot() (woken, backs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.woken), slices.Clone(p.backs)
}

var t0 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// rig is a core on a fake source and pool, with a clock the test moves.
type rig struct {
	t      *testing.T
	core   *core.Core
	source *fakeSource
	pool   *fakePool
	now    time.Time
	mu     sync.Mutex
}

func newRig(t *testing.T, cfg core.Config, tenants ...core.Tenant) *rig {
	t.Helper()
	r := &rig{t: t, now: t0}
	ids := make([]string, len(tenants))
	for i, tn := range tenants {
		ids[i] = tn.ID
	}
	r.source, r.pool = newFakeSource(ids...), newFakePool()
	clk := core.NewClock()
	clk.Set(func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now })
	cfg.Clock = clk
	if cfg.DrainPoll == 0 {
		cfg.DrainPoll = 5 * time.Millisecond
	}
	c, err := core.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Connect(r.source, r.pool)
	r.core = c
	c.SetTenants(context.Background(), tenants)
	return r
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *rig) dispatch() { r.core.DispatchOnce(context.Background()) }

// next is the task the pod picks up, "" when none.
func (r *rig) next(pod string) string {
	raw := r.core.Next(context.Background(), pod)
	if raw == nil {
		return ""
	}
	var t struct{ ID string }
	if err := json.Unmarshal(raw, &t); err != nil {
		r.t.Fatal(err)
	}
	return t.ID
}

func tenant(id string, s policy.TenantSettings) core.Tenant { return core.Tenant{ID: id, Settings: s} }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
