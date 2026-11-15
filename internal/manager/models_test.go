package manager_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The Multica UI asks the runtime which models its agent CLI can run; the
// request arrives in the answer to the manager's own heartbeat. The CLIs are in
// the pods, so one pod's daemon gets the request, asks its CLI and reports to
// the manager, which relays the report with the workspace's account.
func TestModelListRequestIsHandedToOnePodAndItsReportRelayed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	podA := h.newPod("pool-0")
	podB := h.newPod("pool-1")
	podA.start()
	podB.start()

	var mu sync.Mutex
	var reported, reportAuth string
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/api/daemon/heartbeat":
			writeJSON(w, map[string]any{"runtime_id": "runtime-a", "status": "ok", "pending_model_list": map[string]string{"id": "ml-1"}})
			return true
		case strings.HasSuffix(r.URL.Path, "/models/ml-1/result"):
			mu.Lock()
			reported, reportAuth = r.URL.Path, r.Header.Get("Authorization")
			mu.Unlock()
			writeJSON(w, map[string]string{"status": "ok"})
			return true
		}
		return false
	})

	h.pool.HeartbeatOnce(context.Background()) // the manager's own heartbeat brings the request

	status, raw := podA.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if status != 200 || !strings.Contains(string(raw), `"pending_model_list":{"id":"ml-1"}`) {
		t.Fatalf("the first pod's heartbeat should carry the request, got %d %s", status, raw)
	}
	_, other := podB.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	if strings.Contains(string(other), "pending_model_list") {
		t.Fatalf("the request was handed to a second pod: %s", other)
	}

	path := "/api/daemon/runtimes/runtime-a/models/ml-1/result"
	body := map[string]any{"status": "completed", "models": []map[string]string{{"id": "claude-haiku-5-5", "label": "Haiku"}}}
	if status, _ := podB.call(http.MethodPost, path, body); status != http.StatusForbidden {
		t.Fatalf("a pod that was not handed the request reported it: status %d", status)
	}
	if status, _ := podA.call(http.MethodPost, "/api/daemon/runtimes/runtime-a/models/other/result", body); status != http.StatusForbidden {
		t.Fatalf("a report for another request id was relayed: status %d", status)
	}
	if status, raw := podA.call(http.MethodPost, path, body); status != 200 {
		t.Fatalf("the report was refused: %d %s", status, raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if reported != path {
		t.Fatalf("relayed path = %q, want %q", reported, path)
	}
	if reportAuth != "Bearer "+harnessPAT {
		t.Fatalf("the report was not relayed with the workspace account")
	}
	var again map[string]any
	_, raw = podA.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"})
	_ = json.Unmarshal(raw, &again)
	if _, ok := again["pending_model_list"]; ok {
		t.Fatalf("a finished request was handed out again: %s", raw)
	}
}

// A new image can add an agent CLI while the manager keeps running. The pod
// that offers it must get its runtime, not an error that sends its daemon into a
// restart loop.
func TestPodOfferingANewAgentCLIGetsItsRuntimeWithoutAManagerRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 3)
	oldPod := h.newPod("pool-0")
	oldPod.start()
	if h.upstream.registrationCount() != 1 {
		t.Fatalf("registrations after the first pod: %d", h.upstream.registrationCount())
	}

	newPod := h.newPod("pool-1")
	status, raw := newPod.call(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": harnessWorkspaceID, "daemon_id": "pod-pool-1", "device_name": "pool-1",
		"runtimes": []map[string]any{
			{"name": "claude", "type": "claude", "version": "1.0.0", "status": "online"},
			{"name": "opencode", "type": "opencode", "version": "1.19.5", "status": "online"},
		},
	})
	if status != 200 {
		t.Fatalf("registration of a pod with a new CLI: %d %s", status, raw)
	}
	var resp struct {
		Runtimes []struct {
			Provider string `json:"provider"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rt := range resp.Runtimes {
		got = append(got, rt.Provider)
	}
	if len(got) != 2 || !strings.Contains(strings.Join(got, ","), "opencode") {
		t.Fatalf("runtimes handed to the pod = %v, want claude and opencode", got)
	}
	if n := h.upstream.registrationCount(); n != 2 {
		t.Fatalf("upstream registrations = %d, want 2", n)
	}
	// The pod that offers only claude is still served, with no further upstream registration.
	oldPod2 := h.newPod("pool-2")
	if status, _ := oldPod2.register(); status != 200 {
		t.Fatalf("registration of a pod with the old set: %d", status)
	}
	if n := h.upstream.registrationCount(); n != 2 {
		t.Fatalf("a pod with a known set caused an upstream registration: %d", n)
	}
}

// The pods run one image, so one pod's model list is every runtime's. Once a
// pod reported it, the manager answers the next request itself, at once and
// with no pod: a picker opened while every pod is busy, or before any pod of
// the workspace registered, still lists the models.
func TestALaterModelListRequestIsAnsweredFromTheLastReport(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	pod := h.newPod("pool-0")
	pod.start()

	var mu sync.Mutex
	request := "ml-1"
	reports := map[string]string{}
	h.upstream.intercept(func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/daemon/heartbeat":
			writeJSON(w, map[string]any{"runtime_id": "runtime-a", "status": "ok", "pending_model_list": map[string]string{"id": request}})
			return true
		case strings.HasSuffix(r.URL.Path, "/result"):
			var body struct {
				Models []struct {
					ID string `json:"id"`
				} `json:"models"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Models) > 0 {
				reports[r.URL.Path] = body.Models[0].ID
			}
			writeJSON(w, map[string]string{"status": "ok"})
			return true
		}
		return false
	})

	h.pool.HeartbeatOnce(context.Background())
	if _, raw := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"}); !strings.Contains(string(raw), "ml-1") {
		t.Fatalf("with no report yet the request should go to the pod: %s", raw)
	}
	body := map[string]any{"status": "completed", "models": []map[string]string{{"id": "claude-haiku-5-5", "label": "Haiku"}}}
	if status, raw := pod.call(http.MethodPost, "/api/daemon/runtimes/runtime-a/models/ml-1/result", body); status != 200 {
		t.Fatalf("the report was refused: %d %s", status, raw)
	}

	mu.Lock()
	request = "ml-2"
	mu.Unlock()
	h.pool.HeartbeatOnce(context.Background())
	mu.Lock()
	got := reports["/api/daemon/runtimes/runtime-a/models/ml-2/result"]
	mu.Unlock()
	if got != "claude-haiku-5-5" {
		t.Fatalf("the second request was not answered from the last report: reports %v", reports)
	}
	if _, raw := pod.call(http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": "runtime-a"}); strings.Contains(string(raw), "pending_model_list") {
		t.Fatalf("an answered request was still handed to a pod: %s", raw)
	}
}
