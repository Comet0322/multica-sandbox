// Package dispatcher runs the sandbox pool manager: it registers one runtime
// per Multica workspace and runs the tasks of those runtimes in a fixed pool of
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
package dispatcher

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an http(s) base URL")
	}
	return nil
}

type proxyContextKey struct{}

// proxyRoute carries per-request forwarding decisions into the reverse proxy.
type proxyRoute struct {
	// managerAuth replaces the pod's credential with the bot account's ticket.
	managerAuth bool
	// ticket is the credential used when managerAuth is set: the bot
	// account's PAT, the same in every workspace.
	ticket string
	// after runs with the upstream status code of a forwarded call.
	after func(status int)
}

// runtimeRef is a registered runtime as the server reports it.
type runtimeRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var okBody = map[string]string{"status": "ok"}

// taskTokenPath matches daemon endpoints the server authorizes with a
// task-scoped token carried in the claim (RemoteMCPDaemonToken).
func taskTokenPath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/api/daemon/"), "/")
	if len(parts) < 3 || parts[0] != "tasks" {
		return false
	}
	return parts[2] == "plugin-hooks" || (len(parts) == 5 && parts[4] == "credential")
}
