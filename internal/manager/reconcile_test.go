package manager_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

// Multica knows how a task ended even when the pod never says.
func TestReconcileEndsTheLeasesOfTasksThatAreOver(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		code   int
		status string
		ended  string // empty: the lease stays
	}{
		{"completed", 0, "completed", "upstream task ended: completed"},
		{"failed", 0, "failed", "upstream task ended: failed"},
		{"cancelled", 0, "cancelled", "upstream task ended: cancelled"},
		{"running", 0, "running", ""},
		{"dispatched", 0, "dispatched", ""},
		{"gone", http.StatusNotFound, "", "upstream task ended: not found"},
		{"token revoked", http.StatusUnauthorized, "", "upstream access lost (HTTP 401)"},
		{"workspace left", http.StatusForbidden, "", "upstream access lost (HTTP 403)"},
		{"server error", http.StatusInternalServerError, "", ""},
		{"overloaded", http.StatusTooManyRequests, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, 1)
			pod := h.newPod("pool-0")
			leaseOne(t, h, pod, "t1")
			answerTask(h.upstream, func(id, a string) (int, string) {
				if a == "status" {
					return tc.code, tc.status
				}
				return 0, ""
			})

			h.pool.ReconcileOnce(context.Background())

			if tc.ended == "" {
				h.wantHeld("t1")
			} else {
				h.wantEnded("t1", tc.ended)
				if inv := h.inventory(); inv.Resetting != 1 {
					t.Fatalf("the pod must be reset: %+v", inv)
				}
			}
			if n := h.upstream.callCount("POST /api/daemon/tasks/t1/fail"); n != 0 {
				t.Fatalf("a task Multica already closed is not failed again (%d fail calls)", n)
			}
		})
	}
}

// A status that errors one pass and answers the next ends the lease then.
func TestReconcileRetriesOnTheNextPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	var failing atomic.Bool
	failing.Store(true)
	answerTask(h.upstream, func(id, a string) (int, string) {
		if a != "status" {
			return 0, ""
		}
		if failing.Load() {
			return http.StatusInternalServerError, ""
		}
		return 0, "completed"
	})
	h.pool.ReconcileOnce(context.Background())
	h.wantHeld("t1")
	failing.Store(false)
	h.pool.ReconcileOnce(context.Background())
	h.wantEnded("t1", "upstream task ended: completed")
}

// The reconcile calls Multica without holding the pool's lock, and a Multica
// that never answers cannot hold the lease or the pool.
func TestReconcileHoldsNoLockWhileMulticaIsSlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/status" {
			return false
		}
		select {
		case asked <- struct{}{}:
		default:
		}
		select {
		case <-release:
			writeJSON(w, map[string]any{"status": "completed"})
		case <-r.Context().Done():
		}
		return true
	})

	done := make(chan struct{})
	go func() { h.pool.ReconcileOnce(context.Background()); close(done) }()
	<-asked
	// Everything else keeps answering while the lookup hangs.
	answered := make(chan struct{})
	go func() {
		h.inventory()
		if status := pod.heartbeat(); status != http.StatusOK {
			t.Errorf("heartbeat during a slow reconcile: status %d", status)
		}
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(3 * time.Second):
		t.Fatal("the pool stopped answering while a reconcile waited for Multica")
	}
	h.wantHeld("t1")
	close(release)
	<-done
	h.wantEnded("t1", "upstream task ended: completed")
}

func TestReconcileGivesUpOnAStatusCallWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/status" {
			return false
		}
		<-r.Context().Done() // never answers
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.pool.ReconcileOnce(ctx)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the reconcile hung for %s", took)
	}
	h.wantHeld("t1")
}

func TestReconcileAsksAtMostEightAtOnce(t *testing.T) {
	t.Parallel()
	const n = 20
	h := newHarness(t, n)
	for i := 0; i < n; i++ {
		pod := h.newPod(fmt.Sprintf("pool-%d", i))
		leaseOne(t, h, pod, fmt.Sprintf("t%d", i))
	}
	var inflight, peak atomic.Int32
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			return false
		}
		cur := inflight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		inflight.Add(-1)
		writeJSON(w, map[string]any{"status": "running"})
		return true
	})
	h.pool.ReconcileOnce(context.Background())
	if p := peak.Load(); p > 8 || p < 2 {
		t.Fatalf("status calls in flight at once: peak %d, want 2..8", p)
	}
}

// The pod reports the task done while the reconcile is still asking; the
// lease must end once, with the pod's reason.
func TestReconcileDoesNotEndALeaseThatEndedMeanwhile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/status" {
			return false
		}
		asked <- struct{}{}
		<-release
		writeJSON(w, map[string]any{"status": "completed"})
		return true
	})
	done := make(chan struct{})
	go func() { h.pool.ReconcileOnce(context.Background()); close(done) }()
	<-asked
	if status := pod.report("t1", "complete"); status != http.StatusOK {
		t.Fatalf("complete: status %d", status)
	}
	close(release)
	<-done

	var records int
	for _, l := range h.leases() {
		if l.TaskID == "t1" {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("want one lease record, got %d", records)
	}
	h.wantEnded("t1", "pod reported complete")
	if n := strings.Count(h.audit.String(), `"reason":"pod reported complete"`) + strings.Count(h.audit.String(), "upstream task ended"); n > 1 {
		t.Fatalf("the lease was released twice:\n%s", h.audit.String())
	}
}

// Run starts the reconcile only when ReconcileInterval says so.
func TestRunReconcilesOnItsIntervalAndNotWithoutOne(t *testing.T) {
	t.Parallel()
	statusCalls := func(h *harness) int { return h.upstream.callCount("GET /api/daemon/tasks/t1/status") }
	t.Run("on", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1, func(c *manager.Config) { c.ReconcileInterval = 20 * time.Millisecond })
		pod := h.newPod("pool-0")
		leaseOne(t, h, pod, "t1")
		answerTask(h.upstream, func(id, a string) (int, string) {
			if a == "status" {
				return 0, "cancelled"
			}
			return 0, ""
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.pool.Run(ctx)
		eventually(t, "the loop to end the lease", func() bool { return h.lease("t1").EndedAt != nil })
		h.wantEnded("t1", "upstream task ended: cancelled")
	})
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1)
		pod := h.newPod("pool-0")
		leaseOne(t, h, pod, "t1")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.pool.Run(ctx) // dispatches, but does not reconcile
		time.Sleep(150 * time.Millisecond)
		if n := statusCalls(h); n != 0 {
			t.Fatalf("a reconcile ran without an interval: %d status calls", n)
		}
		h.wantHeld("t1")
	})
}

// Whatever happens, a lease ends at LeaseMax and its task is failed.
func TestLeaseMaxEndsAnOldLeaseAndFailsItsTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, farHeartbeat, func(c *manager.Config) { c.LeaseMax = 10 * time.Minute })
	clock := h.withClock()
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")

	clock.advance(10*time.Minute - time.Second)
	h.pool.Touch()
	h.wantHeld("t1")

	clock.advance(2 * time.Second)
	h.pool.Touch()
	h.wantEnded("t1", "lease exceeded the maximum lease time")
	if status := pod.heartbeat(); status != http.StatusOK && status != http.StatusNotFound {
		t.Fatalf("heartbeat: status %d", status)
	}
	eventually(t, "the task to be failed upstream", func() bool {
		msg, ok := h.upstream.failureOf("t1")
		return ok && msg == "task exceeded the maximum lease time"
	})
}

func TestLeaseMaxDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	t.Run("two hours by default", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, 1, farHeartbeat)
		clock := h.withClock()
		pod := h.newPod("pool-0")
		leaseOne(t, h, pod, "t1")
		clock.advance(2*time.Hour - time.Minute)
		h.pool.Touch()
		h.wantHeld("t1")
		clock.advance(2 * time.Minute)
		h.pool.Touch()
		h.wantEnded("t1", "lease exceeded the maximum lease time")
	})
	t.Run("negative is refused", func(t *testing.T) {
		t.Parallel()
		_, err := manager.New(manager.Config{
			MulticaURL: "http://127.0.0.1:1", PAT: harnessPAT, LeaseMax: -time.Second,
		}, nil)
		if err == nil {
			t.Fatal("a negative LeaseMax must be refused")
		}
	})
}

// A failure report that Multica could not take is sent again.
func TestFailureReportThatMulticaCouldNotTakeIsRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	var down atomic.Bool
	down.Store(true)
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/daemon/tasks/t1/fail" && down.Load() {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true
		}
		return false
	})

	if status := pod.event("daemon-exited"); status != http.StatusOK {
		t.Fatalf("event: status %d", status)
	}
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("the unsent failure must be queued, pending %d", n)
	}
	// Reconciling while Multica is still down keeps it queued, once.
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("pending %d after a failed retry", n)
	}

	down.Store(false)
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 0 {
		t.Fatalf("pending %d after Multica took it", n)
	}
	if msg, ok := h.upstream.failureOf("t1"); !ok || msg != "the pod's daemon restarted" {
		t.Fatalf("failure upstream = %q, %v", msg, ok)
	}
}

func TestFailureReportMulticaRefusesIsDropped(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusConflict, http.StatusNotFound, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, 1)
			pod := h.newPod("pool-0")
			leaseOne(t, h, pod, "t1")
			var first atomic.Bool
			h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/daemon/tasks/t1/fail" {
					return false
				}
				if first.CompareAndSwap(false, true) {
					http.Error(w, "bad gateway", http.StatusBadGateway)
				} else {
					http.Error(w, "closed", code)
				}
				return true
			})
			pod.event("daemon-exited")
			if n := h.inventory().PendingFailReports; n != 1 {
				t.Fatalf("pending %d", n)
			}
			h.pool.ReconcileOnce(context.Background())
			if n := h.inventory().PendingFailReports; n != 0 {
				t.Fatalf("a refusal means Multica has dealt with the task; pending %d", n)
			}
			// A refusal at the first attempt is not queued at all.
			h2 := newHarness(t, 1)
			pod2 := h2.newPod("pool-0")
			leaseOne(t, h2, pod2, "t2")
			h2.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/fail") {
					http.Error(w, "closed", code)
					return true
				}
				return false
			})
			pod2.event("daemon-exited")
			if n := h2.inventory().PendingFailReports; n != 0 {
				t.Fatalf("a first-attempt refusal was queued: %d", n)
			}
		})
	}
}

func TestFailureReportIsGivenUpAfterThirtyMinutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1, farHeartbeat)
	clock := h.withClock()
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true
		}
		return false
	})
	pod.event("daemon-exited")
	clock.advance(29 * time.Minute)
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("still within 30 minutes, pending %d", n)
	}
	clock.advance(2 * time.Minute)
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 0 {
		t.Fatalf("pending %d after 30 minutes", n)
	}
	if !strings.Contains(h.logs.String(), "gave up failing task upstream") {
		t.Fatalf("giving up must be logged as an error:\n%s", h.logs.String())
	}
}

func TestTaskIsQueuedForRetryOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true
		}
		return false
	})
	pod.event("daemon-exited")
	pod.event("daemon-exited")
	h.pool.ReconcileOnce(context.Background())
	h.pool.ReconcileOnce(context.Background())
	if n := h.inventory().PendingFailReports; n != 1 {
		t.Fatalf("one task, one queued report; got %d", n)
	}
}

// The failure still goes out when the request that caused it is abandoned.
func TestFailureReportSurvivesTheCancelledRequestThatCausedIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	var delivered atomic.Bool
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/daemon/tasks/t1/fail" {
			return false
		}
		time.Sleep(400 * time.Millisecond)
		if r.Context().Err() == nil {
			delivered.Store(true)
		}
		return false
	})
	short := &http.Client{Transport: pod.client.Transport, Timeout: 100 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodPost, h.podAPI.URL+"/pod/control", strings.NewReader(`{"event":"daemon-exited"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pod.token)
	_, _ = short.Do(req) // gives up before the manager is done
	eventually(t, "the failure to reach Multica", func() bool { return delivered.Load() })
}
