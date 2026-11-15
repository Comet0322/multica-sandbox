package devpool_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/multica/devpool"
)

type fakeCore struct {
	mu     sync.Mutex
	queue  map[string][]json.RawMessage
	owned  map[string]string
	ended  []string
	serves []string
}

func (c *fakeCore) Next(_ context.Context, pod string) json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queue[pod]
	if len(q) == 0 {
		return nil
	}
	c.queue[pod] = q[1:]
	var t struct{ ID string }
	_ = json.Unmarshal(q[0], &t)
	c.owned[t.ID] = pod
	return q[0]
}

func (c *fakeCore) Owns(pod, task string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owned[task] == pod
}

func (c *fakeCore) TaskEnded(pod, task, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = append(c.ended, pod+" "+task+" "+reason)
}

func (c *fakeCore) PodServes(pod, tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serves = append(c.serves, pod+" "+tenant)
}

type fakeSource struct{}

func (fakeSource) Tenants() []multica.WorkspaceRef {
	return []multica.WorkspaceRef{{ID: "ws-1", Name: "One"}}
}

func (fakeSource) Register(_ context.Context, _ string, req multica.Registration) (map[string]json.RawMessage, string, *multica.PodError) {
	return map[string]json.RawMessage{}, req.WorkspaceID, nil
}

func TestASimulatedPodRunsItsTaskAndReportsIt(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &fakeCore{queue: map[string][]json.RawMessage{}, owned: map[string]string{}}
	p := devpool.New(devpool.Config{Pods: 2, TaskMin: 10 * time.Millisecond, Poll: 10 * time.Millisecond}, c, fakeSource{}, multica.NewClient(srv.URL, "mul_pat", "devpool", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	id, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.queue[id] = append(c.queue[id], json.RawMessage(`{"id":"t1","runtime_id":"rt","dispatched_at":"x","start_claim_supported":true}`))
	c.mu.Unlock()
	p.Wake(id)

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		ended := slices.Clone(c.ended)
		serves := len(c.serves)
		c.mu.Unlock()
		if len(ended) == 1 {
			if ended[0] != id+" t1 pod reported complete" || serves != 2 {
				t.Fatalf("ended %v, %d registrations", ended, serves)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the task did not end; calls %v", calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"/api/daemon/tasks/t1/start", "/api/daemon/tasks/t1/complete"}) {
		t.Fatalf("upstream calls %v", calls)
	}
}

func TestAHandedBackPodIsReplacedByANewOne(t *testing.T) {
	p := devpool.New(devpool.Config{Pods: 1, StartTime: time.Hour}, &fakeCore{}, fakeSource{}, multica.NewClient("http://127.0.0.1:1", "x", "d", nil))
	id, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(context.Background()); err != core.ErrNoCapacity {
		t.Fatalf("a second acquire of the only pod: %v", err)
	}
	_ = p.HandBack(context.Background(), id, core.HandBack{Reason: "done"})
	if _, ok := p.Pod(id); ok {
		t.Fatal("the handed-back pod is still listed")
	}
	pods := p.Pods()
	if len(pods) != 1 || pods[0].ID == id || pods[0].State != core.PodStarting {
		t.Fatalf("pods %+v, want one new pod starting", pods)
	}
}
