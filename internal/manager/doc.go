// Package manager runs the sandbox manager: it registers one runtime per
// Multica workspace and runs the tasks of those runtimes in a fixed pool of
// pods, each running the stock Multica daemon. See deploy/POOL.md.
//
// Upstream, the manager is an ordinary daemon: it registers runtimes,
// heartbeats and claims tasks. Downstream, it impersonates the Multica server
// for the daemon inside each pod: register, heartbeat and claim are answered
// locally, task-scoped calls are forwarded with the workspace's credential after
// an ownership check, and everything else under /api/ passes through untouched
// so agents' task tokens keep working. The daemon code is never modified.
//
// Tasks are claimed upstream only on the dispatch tick, against a pod the
// manager holds; a daemon's claim only picks up a task placed on its pod. The
// pods themselves (registration, pod tokens, control channel, resets,
// liveness) belong to the sandbox service in package sandbox, which the
// manager reaches through its Create and Delete.
package manager

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var okBody = map[string]string{"status": "ok"}
