package core_test

import (
	"cmp"
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
	// onFail, when set, is called with each failure report as it arrives,
	// before Fail answers; it may block.
	onFail func(taskID string)
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
	onFail := s.onFail
	s.mu.Unlock()
	if onFail != nil {
		onFail(taskID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, tenant+" "+taskID+" "+reason)
	return s.failErr
}

// whenFail sets onFail.
func (s *fakeSource) whenFail(fn func(taskID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onFail = fn
}

// end makes TaskEnded report the task over, for reason.
func (s *fakeSource) end(taskID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended[taskID] = reason
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
	adopted []core.HeldPod
}

type fakePod struct {
	id      string
	state   core.PodState
	tenants []string
}

func newFakePool() *fakePool {
	return &fakePool{preferd: map[string]bool{}}
}

// Adopt holds the pods it is given that the pool has and are not lost, and
// deletes every other pod, as a pool that restarted with the core does.
func (p *fakePool) Adopt(_ context.Context, held []core.HeldPod) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adopted = held
	keep := map[string]bool{}
	for _, h := range held {
		keep[h.ID] = true
	}
	var out []*fakePod
	for _, pd := range p.pods {
		if keep[pd.id] && pd.state != core.PodLost {
			pd.state = core.PodHeld
			out = append(out, pd)
		}
	}
	p.pods = out
	return nil
}

// ids lists the pods the pool has, in order.
func (p *fakePool) ids() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, pd := range p.pods {
		out = append(out, pd.id)
	}
	return out
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

// fakeStore keeps the dispatch record in memory as a database would: the
// latest row of each ID, a payload only while its task is placed or handed,
// no email.
type fakeStore struct {
	mu    sync.Mutex
	pods  map[string]core.PodRow
	tasks map[string]core.TaskRow
	// err is what Write returns; nil writes.
	err error
	// hold, when set, holds each Write until it receives from it; entered
	// is told of each Write as it starts.
	hold, entered chan struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{pods: map[string]core.PodRow{}, tasks: map[string]core.TaskRow{}}
}

func (s *fakeStore) Load(context.Context) (core.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec core.Record
	for _, p := range s.pods {
		if p.GoneAt.IsZero() {
			rec.Pods = append(rec.Pods, p)
		}
	}
	slices.SortFunc(rec.Pods, func(a, b core.PodRow) int {
		return cmp.Or(a.ReadyAt.Compare(b.ReadyAt), cmp.Compare(a.ID, b.ID))
	})
	for _, t := range s.tasks {
		if t.State != core.TaskEnded || t.FailPending {
			rec.Tasks = append(rec.Tasks, t)
		}
	}
	slices.SortFunc(rec.Tasks, func(a, b core.TaskRow) int {
		return cmp.Or(a.ClaimedAt.Compare(b.ClaimedAt), cmp.Compare(a.ID, b.ID))
	})
	return rec, nil
}

func (s *fakeStore) Write(_ context.Context, pods []core.PodRow, tasks []core.TaskRow) error {
	s.mu.Lock()
	hold, entered := s.hold, s.entered
	s.mu.Unlock()
	if hold != nil {
		entered <- struct{}{}
		<-hold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for _, p := range pods {
		s.pods[p.ID] = p
	}
	for _, t := range tasks {
		t.Email = ""
		if t.State != core.TaskPlaced && t.State != core.TaskHanded {
			t.Payload = nil
		}
		s.tasks[t.ID] = t
	}
	return nil
}

// failWith makes Write fail with err, nil to write again.
func (s *fakeStore) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// holdWrites makes every Write wait until letWrite or releaseWrites.
func (s *fakeStore) holdWrites() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.entered = make(chan struct{}), make(chan struct{}, 100)
}

// writeStarted waits for a held Write to start.
func (s *fakeStore) writeStarted(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	entered := s.entered
	s.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a write")
	}
}

// letWrite lets one held Write through.
func (s *fakeStore) letWrite() {
	s.mu.Lock()
	hold := s.hold
	s.mu.Unlock()
	hold <- struct{}{}
}

// releaseWrites lets every Write through again.
func (s *fakeStore) releaseWrites() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil {
		close(s.hold)
		s.hold, s.entered = nil, nil
	}
}

func (s *fakeStore) task(id string) core.TaskRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id]
}

func (s *fakeStore) pod(id string) core.PodRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pods[id]
}

var t0 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// rig is a core on a fake source, pool and store, with a clock the test
// moves.
type rig struct {
	t       *testing.T
	cfg     core.Config
	tenants []core.Tenant
	core    *core.Core
	source  *fakeSource
	pool    *fakePool
	store   *fakeStore
	now     time.Time
	mu      sync.Mutex
}

func newRig(t *testing.T, cfg core.Config, tenants ...core.Tenant) *rig {
	t.Helper()
	r := &rig{t: t, cfg: cfg, tenants: tenants, now: t0, store: newFakeStore()}
	ids := make([]string, len(tenants))
	for i, tn := range tenants {
		ids[i] = tn.ID
	}
	r.source, r.pool = newFakeSource(ids...), newFakePool()
	r.core = r.build()
	r.core.SetTenants(context.Background(), tenants)
	return r
}

// build makes a core on the rig's source, pool, store and clock.
func (r *rig) build() *core.Core {
	r.t.Helper()
	cfg := r.cfg
	clk := core.NewClock()
	clk.Set(func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now })
	cfg.Clock, cfg.Store = clk, r.store
	if cfg.DrainPoll == 0 {
		cfg.DrainPoll = 5 * time.Millisecond
	}
	c, err := core.New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	c.Connect(r.source, r.pool)
	return c
}

// restart is the manager stopping and starting again: the core writes what
// changed last, then a new core starts on the same store and source, with a
// new pool that finds every pod not lost, as Kubernetes keeps them. The core
// restores, then learns its tenants.
func (r *rig) restart() {
	r.t.Helper()
	r.flush()
	old := r.pool
	r.pool = newFakePool()
	old.mu.Lock()
	for _, pd := range old.pods {
		if pd.state != core.PodLost {
			r.pool.pods = append(r.pool.pods, &fakePod{id: pd.id, state: core.PodFree, tenants: pd.tenants})
		}
	}
	old.mu.Unlock()
	r.core = r.build()
	if err := r.core.Restore(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	r.core.SetTenants(context.Background(), r.tenants)
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *rig) dispatch() { r.core.DispatchOnce(context.Background()) }

// flush waits until the store holds every change the core made.
func (r *rig) flush() {
	r.t.Helper()
	if err := r.core.Flush(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// wantFails waits until the source got exactly these failure reports, which
// go out once the record holds them.
func (r *rig) wantFails(want ...string) {
	r.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, fails, _ := r.source.snapshot()
		if slices.Equal(fails, want) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("fails = %q, want %q", fails, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

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
