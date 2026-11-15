package manager_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"sandbox-manager/internal/manager"
)

func (h *harness) dispatchView() manager.DispatchView {
	h.t.Helper()
	resp, err := http.Get(h.admin.URL + "/v1/dispatch")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("dispatch status %d", resp.StatusCode)
	}
	var v manager.DispatchView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		h.t.Fatal(err)
	}
	return v
}

func TestDispatchViewShowsTargetAndActualPods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 2)
	pod := h.newPod("pool-0")
	leaseOne(t, h, pod, "t1")
	h.newPod("pool-1").start()

	v := h.dispatchView()
	if v.Pool.Live != 2 || v.Pool.Expected != 2 {
		t.Fatalf("pool %+v, want 2 live of 2 expected", v.Pool)
	}
	if len(v.Workspaces) != 1 {
		t.Fatalf("workspaces %+v", v.Workspaces)
	}
	w := v.Workspaces[0]
	if w.OccupiedPods != 1 || w.RunningPods != 1 || w.HoldingPods != 0 || w.TargetPods != 1 || w.Leased != 1 || w.Weight != 1 {
		t.Fatalf("workspace share %+v", w)
	}
	if v.Totals.OccupiedPods != 1 || v.Totals.UnoccupiedPods != 1 {
		t.Fatalf("totals %+v", v.Totals)
	}
	if len(v.Pods) != 2 {
		t.Fatalf("pods %+v", v.Pods)
	}
	byState := map[string]manager.DispatchPod{}
	for _, p := range v.Pods {
		byState[p.State] = p
	}
	if p := byState[manager.PodLeased]; p.WorkspaceID != w.WorkspaceID || p.Tasks != 1 || p.Hold {
		t.Fatalf("the leased pod %+v", p)
	}
	if p, ok := byState[manager.PodIdle]; !ok || p.WorkspaceID != "" {
		t.Fatalf("the idle pod %+v", p)
	}
}

func TestDispatchPageReadsTheViewItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 1)
	resp, err := http.Get(h.admin.URL + "/dispatch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), `fetch("v1/dispatch"`) || !strings.Contains(string(body), "10000") {
		t.Fatal("the page must read /v1/dispatch every 10 seconds")
	}
}
