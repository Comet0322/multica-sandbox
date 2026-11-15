package manager_test

import (
	"fmt"
	"net/http"
	"testing"
)

// A cancelled task is never reported complete or failed by the stock
// daemon, only acknowledged. The acknowledgement has to end the lease, or the
// pod stays leased until the pod dies.
func TestCancelAckEndsTheLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")

	if status := pod.report("t1", "cancel-ack"); status != http.StatusOK {
		t.Fatalf("cancel-ack: status %d", status)
	}
	h.wantEnded("t1", "pod reported cancel-ack")
	if inv := h.inventory(); inv.Idle != 1 || inv.Running != 0 {
		t.Fatalf("the pod must be idle after the lease: %+v", inv)
	}
	if _, failed := h.upstream.failureOf("t1"); failed {
		t.Fatalf("a cancelled task must not be failed upstream")
	}
}

func TestTerminalReportMulticaCouldNotTakeKeepsTheLease(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel-ack", "complete", "fail"} {
		for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s %d", action, code), func(t *testing.T) {
				t.Parallel()
				h := newHarness(t, 1)
				pod := h.newPod("pool-0")
				leaseOne(t, h, pod, "t1")
				answerTask(h.upstream, func(id, a string) (int, string) {
					if a == action {
						return code, ""
					}
					return 0, ""
				})
				if status := pod.report("t1", action); status != code {
					t.Fatalf("the pod must see Multica's answer %d, got %d", code, status)
				}
				h.wantHeld("t1")
			})
		}
	}
}

// Multica answering 401/403/404 to a terminal report means the task
// is gone for this token; the lease ends and the task is not failed again.
func TestTerminalReportMulticaRefusedEndsTheLease(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel-ack", "complete", "fail"} {
		for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s %d", action, code), func(t *testing.T) {
				t.Parallel()
				h := newHarness(t, 1)
				pod := h.newPod("pool-0")
				leaseOne(t, h, pod, "t1")
				answerTask(h.upstream, func(id, a string) (int, string) {
					if a == action {
						return code, ""
					}
					return 0, ""
				})
				pod.report("t1", action)
				h.wantEnded("t1", fmt.Sprintf("upstream refused the pod's %s report (HTTP %d)", action, code))
				if n := h.upstream.callCount("POST /api/daemon/tasks/t1/fail"); action != "fail" && n != 0 {
					t.Fatalf("the manager must not fail the task again, %d fail calls", n)
				}
				if inv := h.inventory(); inv.Idle != 1 {
					t.Fatalf("the pod must be idle: %+v", inv)
				}
			})
		}
	}
}

func TestReportThatIsNotTerminalNeverEndsTheLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	answerTask(h.upstream, func(id, a string) (int, string) { return http.StatusNotFound, "" })
	for _, action := range []string{"start", "progress", "messages"} {
		if status := pod.report("t1", action); status != http.StatusNotFound {
			t.Fatalf("%s: status %d", action, status)
		}
	}
	h.wantHeld("t1")
}
