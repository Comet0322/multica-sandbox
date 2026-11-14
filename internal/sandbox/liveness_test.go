package sandbox_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

func shortHeartbeat(c *sandbox.Config) { c.HeartbeatTimeout = 3 * time.Second }

func TestControlChannelTrafficKeepsAPodLive(t *testing.T) {
	h := newHarness(t, shortHeartbeat)
	held := h.registerClean("pool-0")
	free := h.registerClean("pool-1")
	if got := h.mustCreate(); got != held.id {
		t.Fatalf("setup: create returned %q", got)
	}

	for i := range 5 {
		h.advance(time.Second)
		held.poll()
		if i%2 == 0 {
			free.event("something-new") // a report counts as much as a poll
		} else {
			free.poll()
		}
	}
	h.wantState(held, sandbox.PodHeld)
	h.wantState(free, sandbox.PodFree)
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("live pods were reported: %+v", got)
	}
}

func TestASilentHeldPodIsLostAndItsHolderTold(t *testing.T) {
	h := newHarness(t, shortHeartbeat)
	pod := h.registerClean("pool-0")
	h.mustCreate()

	h.advance(3500 * time.Millisecond)
	h.wantState(pod, sandbox.PodLost)
	want := []notice{{"lost", pod.id, "pod lost"}}
	if got := h.rec.takeNotices(); !slices.Equal(got, want) {
		t.Fatalf("notices = %+v, want %+v", got, want)
	}
	h.svc.Inventory()
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("the loss was reported again: %+v", got)
	}
}

// A free pod that went silent may have been interrupted mid-anything: it is
// not handed out, and when it speaks again it is reset first.
func TestASilentFreePodIsUnavailableAndResetWhenItComesBack(t *testing.T) {
	h := newHarness(t, shortHeartbeat)
	pod := h.registerClean("pool-0")

	h.advance(3500 * time.Millisecond)
	h.wantState(pod, sandbox.PodLost)
	h.wantNoCapacity()
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("a pod nobody holds was reported: %+v", got)
	}

	pod.completeReset()
	if got := h.mustCreate(); got != pod.id {
		t.Fatalf("create returned %q, want %q", got, pod.id)
	}
}

func TestAStallOfTheServiceGivesEveryPodOneGracePeriod(t *testing.T) {
	t.Run("a stall gives every pod a grace period", func(t *testing.T) {
		h := newHarness(t)
		pod := h.registerClean("pool-0")
		h.mustCreate()
		h.stall(60 * time.Second)
		h.wantState(pod, sandbox.PodHeld)
		if got := h.rec.takeNotices(); len(got) != 0 {
			t.Fatalf("the pods' silence was the service's own: %+v", got)
		}
		if !strings.Contains(h.logText(), "stalled") {
			t.Fatalf("the stall must be logged:\n%s", h.logText())
		}
		// The grace period is one heartbeat timeout, not more.
		h.advance(sandbox.DefaultHeartbeatTimeout + time.Second)
		h.wantState(pod, sandbox.PodLost)
	})
	t.Run("without a stall a silent pod is lost", func(t *testing.T) {
		h := newHarness(t)
		pod := h.registerClean("pool-0")
		h.advance(60 * time.Second)
		h.wantState(pod, sandbox.PodLost)
	})
}

func pruneTimings(c *sandbox.Config) {
	c.HeartbeatTimeout = 2 * time.Second
	c.LostRetention = 10 * time.Second
}

func TestALostPodStaysListedUntilItsRetentionThenIsPrunedAndItsTokenRefused(t *testing.T) {
	h := newHarness(t, pruneTimings)
	pod := h.registerClean("pool-0")

	h.advance(3 * time.Second)
	if inv := h.svc.Inventory(); len(inv.Pods) != 1 || inv.Pods[0].State != sandbox.PodLost || inv.PrunedTotal != 0 {
		t.Fatalf("a freshly lost pod must stay listed: %+v", inv)
	}

	h.advance(11 * time.Second)
	if inv := h.svc.Inventory(); len(inv.Pods) != 0 || inv.PrunedTotal != 1 {
		t.Fatalf("want the lost pod pruned and counted: %+v", inv)
	}
	if _, ok := h.svc.Authenticate(pod.token); ok {
		t.Fatal("a pruned pod's token still authenticates")
	}
	// Its supervisor then exits so the container restarts and registers anew.
	if status, _ := h.do(http.MethodGet, "/pod/control", pod.token, nil); status != http.StatusUnauthorized {
		t.Fatalf("a pruned pod polling: status %d, want 401", status)
	}
}

func TestADefaultRetentionKeepsALostPodListedForMinutes(t *testing.T) {
	h := newHarness(t, func(c *sandbox.Config) { c.HeartbeatTimeout = 2 * time.Second })
	pod := h.registerClean("pool-0")
	h.advance(3 * time.Second)
	h.svc.Inventory()
	h.advance(time.Minute)
	h.wantState(pod, sandbox.PodLost)
	h.advance(sandbox.DefaultLostRetention)
	if inv := h.svc.Inventory(); len(inv.Pods) != 0 {
		t.Fatalf("want the lost pod pruned after the default retention: %+v", inv)
	}
}

func TestNegativeTimingsAreRejected(t *testing.T) {
	for _, set := range []func(*sandbox.Config){
		func(c *sandbox.Config) { c.HeartbeatTimeout = -time.Second },
		func(c *sandbox.Config) { c.ResetTimeout = -time.Second },
		func(c *sandbox.Config) { c.LostRetention = -time.Second },
	} {
		var cfg sandbox.Config
		set(&cfg)
		if _, err := sandbox.NewGitOps(cfg); err == nil {
			t.Errorf("config %+v was accepted", cfg)
		}
	}
}
