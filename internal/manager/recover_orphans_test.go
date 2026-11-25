package manager_test

import (
	"context"
	"net/http"
	"testing"
)

// The manager never asks Multica to fail what Multica shows on its
// runtimes: its record knows every task it holds, and a task claimed but
// never recorded is Multica's to hand out again. A pod's daemon asking for
// the same is answered here and not relayed: upstream it would fail every
// other pod's tasks on the shared runtime.
func TestTheManagerNeverRecoversOrphansUpstream(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	if _, err := h.pool.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := h.newPod("pool-0")
	pod.start()
	h.newPod("pool-1").start()
	if status, _ := pod.call(http.MethodPost, "/api/daemon/runtimes/runtime-a/recover-orphans", map[string]any{}); status != http.StatusOK {
		t.Fatalf("a pod's recover-orphans: %d, want 200", status)
	}
	if n := h.upstream.callsTo("POST /api/daemon/runtimes/runtime-a/recover-orphans"); n != 0 {
		t.Fatalf("recover-orphans reached Multica %d times, want never", n)
	}
}
