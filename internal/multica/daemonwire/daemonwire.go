// Package daemonwire holds the daemon protocol types the manager exchanges
// with Multica and with the pods' stock daemons. They are our own copy of the
// fields we use from Multica's server/pkg/protocol at v0.6.4, the version the
// company server runs, so that this module does not depend on Multica's; the
// golden fixtures in testdata pin the wire format.
package daemonwire

import "encoding/json"

// Event names of the daemon WebSocket.
const (
	EventDaemonTaskAvailable     = "daemon:task_available"
	EventDaemonWorkspacesChanged = "daemon:workspaces_changed"
)

// Message is one daemon WebSocket frame.
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// TaskAvailablePayload is the payload of EventDaemonTaskAvailable.
type TaskAvailablePayload struct {
	RuntimeID string `json:"runtime_id"`
	TaskID    string `json:"task_id,omitempty"`
}

// DaemonHeartbeatAckPayload is Multica's answer to a runtime heartbeat. Only
// the fields the manager reads are kept; the rest are ignored on decode.
type DaemonHeartbeatAckPayload struct {
	PendingModelList *DaemonHeartbeatPendingModelList `json:"pending_model_list,omitempty"`
}

// DaemonHeartbeatPendingModelList asks the runtime for its model list.
type DaemonHeartbeatPendingModelList struct {
	ID string `json:"id"`
}
