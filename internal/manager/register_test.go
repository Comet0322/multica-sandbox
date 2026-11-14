package manager_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/sandbox"
)

func TestInventoryStartsEmptyWithExpectedSize(t *testing.T) {
	h := newHarness(t, 3)
	inv := h.inventory()
	if inv.ExpectedSize != 3 || inv.Idle != 0 || len(inv.Pods) != 0 {
		t.Fatalf("unexpected empty inventory: %+v", inv)
	}
}

func TestFirstPodRegistersAndShowsIdle(t *testing.T) {
	h := newHarness(t, 2)
	pod := h.newPod("pool-0")

	resp := pod.start()

	if resp["runtimes"] == nil {
		t.Fatalf("registration response has no runtimes: %v", resp)
	}
	inv := h.inventory()
	if inv.Idle != 1 || inv.ExpectedSize != 2 || len(inv.Pods) != 1 {
		t.Fatalf("want 1 idle of 2 expected, got %+v", inv)
	}
	if got := inv.Pods[0]; got.ID != pod.id || got.State != manager.PodIdle || got.Name != "pool-0" {
		t.Fatalf("unexpected pod entry: %+v", got)
	}
	if n := h.upstream.registrationCount(); n != 1 {
		t.Fatalf("manager registered the runtime upstream %d times, want 1", n)
	}
}

// Every daemon and control call is tied to a pod by its token alone.
func TestDaemonAndControlCallsWithoutAKnownPodTokenAreRefused(t *testing.T) {
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()

	calls := []struct{ method, path string }{
		{http.MethodGet, "/api/workspaces"},
		{http.MethodGet, "/api/daemon/workspaces"},
		{http.MethodPost, "/api/tokens/current/renew"},
		{http.MethodPost, "/api/daemon/register"},
		{http.MethodPost, "/api/daemon/heartbeat"},
		{http.MethodPost, "/api/daemon/tasks/claim"},
		{http.MethodPost, "/api/daemon/runtimes/runtime-a/tasks/claim"},
		{http.MethodPost, "/api/daemon/tasks/t1/start"},
		{http.MethodGet, "/api/daemon/ws"},
		{http.MethodGet, "/pod/control"},
		{http.MethodPost, "/pod/control"},
	}
	for _, token := range []string{"", "mpt_forged", "mat_task_token"} {
		stranger := &simPod{h: h, name: "stranger", token: token, client: &http.Client{Timeout: 10 * time.Second}}
		for _, c := range calls {
			if status, body := stranger.call(c.method, c.path, map[string]any{}); status != http.StatusUnauthorized {
				t.Errorf("token %q: %s %s: status %d, want 401: %s", token, c.method, c.path, status, body)
			}
		}
	}
	if n := h.upstream.claimCount(); n != 0 {
		t.Errorf("refused calls caused %d upstream claims", n)
	}
	if inv := h.inventory(); len(inv.Pods) != 1 || inv.Idle != 1 {
		t.Errorf("refused calls changed the pool: %+v", inv)
	}
}

// Behind a mesh every pod reaches the manager from the same address; the
// tokens still tell them apart.
func TestTwoPodsBehindTheSameAddressAreToldApartByTheirTokens(t *testing.T) {
	h := newHarness(t, 2)
	a, b := h.newPod("pool-0"), h.newPod("pool-1")
	a.start()
	b.start()
	h.upstream.enqueue("t1")

	if got := a.claimBatch(); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("pod a: want t1, got %v", got)
	}
	if status := b.report("t1", "start"); status != http.StatusForbidden {
		t.Fatalf("pod b reported on pod a's task: status %d, want 403", status)
	}
	if status := a.report("t1", "start"); status != http.StatusOK {
		t.Fatalf("pod a's own report: status %d", status)
	}
	inv := h.inventory()
	states := map[string]string{}
	for _, p := range inv.Pods {
		states[p.ID] = p.State
	}
	if len(states) != 2 || states[a.id] != manager.PodLeased || states[b.id] != manager.PodIdle {
		t.Fatalf("want pod a leased and pod b idle, got %+v", inv.Pods)
	}
	if n := h.upstream.registrationCount(); n != 1 {
		t.Fatalf("manager registered upstream %d times, want 1", n)
	}
}

// A container restart runs the entrypoint again: the pod registers as a new
// pod with the sandbox service, and the old identity, now silent on its
// control channel, is lost and its task failed.
func TestRestartedContainerRegistersAsANewPodAndTheOldOneIsLost(t *testing.T) {
	h := newHarness(t, 2)
	clk := h.withClock()
	old := h.newPod("pool-0")
	leaseOne(t, h, old, "t1")

	restarted := h.newPod("pool-0")
	restarted.start()
	if restarted.id == old.id {
		t.Fatal("the restarted container kept the old pod identity")
	}
	if got := h.lease("t1"); got.PodID != old.id || got.EndedAt != nil {
		t.Fatalf("the old pod's lease must run until its control channel times out: %+v", got)
	}

	// Only the restarted pod's supervisor polls; the old one stays silent.
	for range 2 {
		clk.advance(sandbox.DefaultHeartbeatTimeout/2 + time.Second)
		restarted.pollControl()
	}
	h.wantEnded("t1", "pod lost")
	if _, ok := h.upstream.failureOf("t1"); !ok {
		t.Fatal("the old pod's task was not failed upstream")
	}
	states := map[string]string{}
	for _, p := range h.inventory().Pods {
		states[p.ID] = p.State
	}
	if states[old.id] != manager.PodLost || states[restarted.id] != manager.PodIdle {
		t.Fatalf("want the old pod lost and the new one idle, got %v", states)
	}
}

func TestNegativeSandboxTimingsAreRejected(t *testing.T) {
	for name, cfg := range map[string]func(*manager.Config){
		"heartbeat timeout": func(c *manager.Config) { c.HeartbeatTimeout = -time.Second },
		"reset timeout":     func(c *manager.Config) { c.ResetTimeout = -time.Second },
		"lost retention":    func(c *manager.Config) { c.LostRetention = -time.Second },
	} {
		c := manager.Config{MulticaURL: "http://127.0.0.1:1", PAT: harnessPAT}
		cfg(&c)
		if _, err := manager.New(c, nil); err == nil {
			t.Errorf("negative %s accepted", name)
		}
	}
}
