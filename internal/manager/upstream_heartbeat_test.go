package manager_test

import (
	"context"
	"testing"
)

func (f *fakeMultica) callsTo(call string) int {
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

// Multica marks a runtime offline, and stops handing out its tasks, when nobody
// heartbeats it. The manager is the one daemon Multica sees, so it heartbeats
// its runtimes itself, on its own timer, whatever the pods do.
func TestTheManagerHeartbeatsItsRuntimesItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	h.pool.HeartbeatOnce(context.Background())
	if got := h.upstream.callsTo("POST /api/daemon/heartbeat"); got != 0 {
		t.Fatalf("heartbeats upstream before any runtime is registered: %d", got)
	}
	pod := h.newPod("pool-0")
	pod.start()
	h.pool.HeartbeatOnce(context.Background())
	if got := h.upstream.callsTo("POST /api/daemon/heartbeat"); got != 1 {
		t.Fatalf("upstream heartbeats for the registered runtime: want 1, got %d", got)
	}
}

// Every pod heartbeats every 15 s; none of it reaches Multica.
func TestPodHeartbeatsAreNotRelayedUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 3)
	for i := range 3 {
		pod := h.newPod("pool-" + string(rune('0'+i)))
		pod.start()
		pod.heartbeat()
		pod.heartbeat()
	}
	if got := h.upstream.callsTo("POST /api/daemon/heartbeat"); got != 0 {
		t.Fatalf("upstream heartbeats for 6 pod heartbeats: want 0, got %d", got)
	}
}
