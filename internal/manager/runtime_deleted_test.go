package manager_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Multica answers a heartbeat with "runtime not found" once the runtime was
// deleted. The manager must forget the registration and make the stock daemon
// register again, which it does only after a 404 from its own heartbeat.
func TestRuntimeDeletedUpstreamMakesThePodRegisterAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()
	var gone atomic.Bool
	gone.Store(true)
	heartbeatAnswer(h.upstream, &gone, http.StatusNotFound, runtimeGoneBody)
	registered := h.upstream.registrationCount()

	h.pool.HeartbeatOnce(context.Background()) // the manager learns the runtime is gone
	eventually(t, "the pod's heartbeat to be refused", func() bool { return pod.heartbeat() == http.StatusNotFound })
	status, body := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if status != http.StatusNotFound || !strings.Contains(string(body), "runtime not found") {
		t.Fatalf("the daemon needs a 404 saying the runtime is gone, got %d %s", status, body)
	}

	// The daemon registers again; the manager registers upstream again.
	gone.Store(false)
	if status, raw := pod.register(); status != http.StatusOK {
		t.Fatalf("register: status %d: %s", status, raw)
	}
	if n := h.upstream.registrationCount(); n != registered+1 {
		t.Fatalf("the workspace must register upstream again: %d -> %d", registered, n)
	}
	// The same runtime id came back; the gone marker must not outlive it, or
	// the daemon would loop on 404.
	if status := pod.heartbeat(); status != http.StatusOK {
		t.Fatalf("heartbeat after the runtime is back: status %d", status)
	}
}

func TestRuntimeDeletedUpstreamComesBackWithANewID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()
	var gone atomic.Bool
	gone.Store(true)
	var renamed atomic.Bool
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/api/daemon/heartbeat" && gone.Load():
			http.Error(w, runtimeGoneBody, http.StatusNotFound)
			return true
		case r.URL.Path == "/api/daemon/register" && renamed.Load():
			writeJSON(w, map[string]any{"runtimes": []map[string]any{{"id": "runtime-b", "provider": "claude", "name": "claude"}}})
			return true
		}
		return false
	})
	h.pool.HeartbeatOnce(context.Background())
	eventually(t, "the runtime to be dropped", func() bool { return pod.heartbeat() == http.StatusNotFound })

	gone.Store(false)
	renamed.Store(true)
	pod.register()
	status, _ := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-b"})
	if status != http.StatusOK {
		t.Fatalf("heartbeat for the new runtime: status %d", status)
	}
	if status := pod.heartbeat(); status != http.StatusNotFound {
		t.Fatalf("the deleted runtime stays refused, got %d", status)
	}
}

func TestAnotherNotFoundDoesNotDropTheRegistration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()
	var on atomic.Bool
	on.Store(true)
	heartbeatAnswer(h.upstream, &on, http.StatusNotFound, `{"error":"not found"}`)
	registered := h.upstream.registrationCount()

	h.pool.HeartbeatOnce(context.Background())
	if status := pod.heartbeat(); status != http.StatusOK {
		t.Fatalf("a 404 that does not say the runtime is gone must not drop it: status %d", status)
	}
	pod.register()
	if n := h.upstream.registrationCount(); n != registered {
		t.Fatalf("the workspace registered upstream again without cause: %d -> %d", registered, n)
	}
}

func TestDroppedRegistrationTellsPodsToLookAgainOnceInAWhile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, withWake(time.Hour), farHeartbeat)
	clock := h.withClock()
	// The harness's first Sync found the workspace and sent the hint; let its
	// rate limit pass.
	clock.advance(31 * time.Second)
	pod := h.newPod("pool-0")
	pod.start()
	sock := pod.openWake()
	var gone atomic.Bool
	gone.Store(true)
	heartbeatAnswer(h.upstream, &gone, http.StatusNotFound, runtimeGoneBody)

	h.pool.HeartbeatOnce(context.Background())
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want %s, got %q", workspacesChangedEvent, got)
	}
	// Registered again, then gone again 11s later: the relay is due, the hint
	// is not (once per 30s).
	gone.Store(false)
	pod.register()
	clock.advance(11 * time.Second)
	gone.Store(true)
	h.pool.HeartbeatOnce(context.Background())
	if got := sock.next(300 * time.Millisecond); got != "" {
		t.Fatalf("a second hint within 30s: %q", got)
	}
	// Later, the hint goes out again.
	gone.Store(false)
	pod.register()
	clock.advance(31 * time.Second)
	gone.Store(true)
	h.pool.HeartbeatOnce(context.Background())
	if got := sock.next(5 * time.Second); got != workspacesChangedEvent {
		t.Fatalf("want a hint after 30s, got %q", got)
	}
}
