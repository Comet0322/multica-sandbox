package sandbox_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

// The service assigns the identity: the label is only shown, and two pods
// with the same label are still two pods with their own tokens.
func TestRegistrationIssuesAPodTokenAndKeepsTheLabelForDisplay(t *testing.T) {
	h := newHarness(t)
	a, b := h.register("pool-0"), h.register("pool-0")

	if a.id == b.id || a.token == b.token {
		t.Fatalf("two registrations got the same identity: %+v %+v", a, b)
	}
	for _, p := range []*simPod{a, b} {
		if !strings.HasPrefix(p.token, "mpt_") || p.id == "pool-0" {
			t.Fatalf("pod identity must be the service's own: %+v", p)
		}
		if id, ok := h.svc.Authenticate(p.token); !ok || id != p.id {
			t.Fatalf("Authenticate(%s) = %q, %v; want %q", p.token, id, ok, p.id)
		}
	}
	inv := h.svc.Inventory()
	if len(inv.Pods) != 2 || inv.Pods[0].ID != a.id || inv.Pods[1].ID != b.id {
		t.Fatalf("want both pods in registration order: %+v", inv.Pods)
	}
	for _, p := range inv.Pods {
		if p.Label != "pool-0" || !p.RegisteredAt.Equal(h.clock.now()) || !p.LastSeen.Equal(h.clock.now()) {
			t.Fatalf("unexpected pod entry: %+v", p)
		}
	}
}

func TestInventoryShowsWhenAPodWasLastSeenOnTheControlChannel(t *testing.T) {
	h := newHarness(t)
	pod := h.registerClean("pool-0")
	registered := h.clock.now()
	h.advance(10 * time.Second)
	pod.poll()
	got := h.svc.Inventory().Pods[0]
	if !got.RegisteredAt.Equal(registered) || !got.LastSeen.Equal(registered.Add(10*time.Second)) {
		t.Fatalf("unexpected times: %+v", got)
	}
}

func TestRegistrationRejectsABadRequest(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.do(http.MethodPost, "/pod/register", "", "not an object"); status != http.StatusBadRequest {
		t.Fatalf("bad body: want 400, got %d", status)
	}
	if status, _ := h.do(http.MethodGet, "/pod/register", "", nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", status)
	}
	if n := len(h.svc.Inventory().Pods); n != 0 {
		t.Fatalf("a rejected registration created %d pods", n)
	}
}

func TestAnOverlongLabelIsCut(t *testing.T) {
	h := newHarness(t)
	h.register(strings.Repeat("x", 1000))
	if got := h.svc.Inventory().Pods[0].Label; len(got) > 260 {
		t.Fatalf("label kept %d bytes", len(got))
	}
}

func TestControlCallsWithoutAKnownPodTokenAreRefused(t *testing.T) {
	h := newHarness(t)
	pod := h.register("pool-0")
	cmd := pod.poll()

	for _, token := range []string{"", "mpt_forged", "mat_task_token"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			body := map[string]string{"id": cmd.ID, "status": "done"}
			if status, raw := h.do(method, "/pod/control", token, body); status != http.StatusUnauthorized {
				t.Errorf("token %q: %s: status %d, want 401: %s", token, method, status, raw)
			}
		}
		if _, ok := h.svc.Authenticate(token); ok {
			t.Errorf("Authenticate(%q) succeeded", token)
		}
	}
	h.wantState(pod, sandbox.PodResetting)
}

func TestTheHandlerServesOnlyThePodPaths(t *testing.T) {
	h := newHarness(t)
	pod := h.register("pool-0")
	if status, _ := h.do(http.MethodGet, "/api/daemon/workspaces", pod.token, nil); status != http.StatusNotFound {
		t.Fatalf("want 404 for a daemon path, got %d", status)
	}
	if status, _ := h.do(http.MethodPut, "/pod/control", pod.token, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 for PUT, got %d", status)
	}
	if status, _ := h.do(http.MethodPost, "/pod/control", pod.token, "not json"); status != http.StatusBadRequest {
		t.Fatalf("want 400 for a bad report, got %d", status)
	}
}
