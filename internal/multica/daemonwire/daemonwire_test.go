package daemonwire_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sandbox-manager/internal/multica/daemonwire"
)

// The fixtures are written in the v0.6.4 wire format of Multica's
// server/pkg/protocol. A type that no longer reads or writes them breaks the
// manager's contract with Multica or with the pods' daemons.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("encoded JSON does not parse: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("got %s, want %s", gb, wb)
	}
}

func TestATaskAvailableFrameRoundTrips(t *testing.T) {
	raw := fixture(t, "message.json")
	var m daemonwire.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != daemonwire.EventDaemonTaskAvailable {
		t.Fatalf("type %q", m.Type)
	}
	var p daemonwire.TaskAvailablePayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.RuntimeID != "rt-1" || p.TaskID != "task-1" {
		t.Fatalf("payload %+v", p)
	}
	out, _ := json.Marshal(m)
	sameJSON(t, out, string(raw))
	out, _ = json.Marshal(p)
	sameJSON(t, out, string(fixture(t, "task_available.json")))
}

func TestAWorkspacesChangedFrameRoundTrips(t *testing.T) {
	raw := fixture(t, "workspaces_changed.json")
	var m daemonwire.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != daemonwire.EventDaemonWorkspacesChanged {
		t.Fatalf("type %q", m.Type)
	}
	out, _ := json.Marshal(daemonwire.Message{Type: daemonwire.EventDaemonWorkspacesChanged})
	sameJSON(t, out, string(raw))
}

func TestAnEmptyTaskAvailablePayloadOmitsTheTask(t *testing.T) {
	out, _ := json.Marshal(daemonwire.TaskAvailablePayload{})
	sameJSON(t, out, `{"runtime_id":""}`)
}

func TestTheHeartbeatAckYieldsThePendingModelList(t *testing.T) {
	var ack daemonwire.DaemonHeartbeatAckPayload
	if err := json.Unmarshal(fixture(t, "heartbeat_ack.json"), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.PendingModelList == nil || ack.PendingModelList.ID != "req-1" {
		t.Fatalf("pending model list %+v", ack.PendingModelList)
	}
	out, _ := json.Marshal(daemonwire.DaemonHeartbeatPendingModelList{ID: "req-1"})
	sameJSON(t, out, `{"id":"req-1"}`)

	var none daemonwire.DaemonHeartbeatAckPayload
	if err := json.Unmarshal([]byte(`{"runtime_id":"rt-1","status":"ok"}`), &none); err != nil {
		t.Fatal(err)
	}
	if none.PendingModelList != nil {
		t.Fatal("an ack without a request yields one")
	}
}
