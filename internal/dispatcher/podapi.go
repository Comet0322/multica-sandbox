package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"sandbox-manager/internal/daemonwire"
	"sandbox-manager/internal/sandbox"
)

// podAPI is the Pod API module: it plays the Multica server toward the stock
// daemon and the supervisor in each pod. It decodes and answers their HTTP
// calls, holds their wake sockets and the model list requests handed to them,
// and asks the Dispatcher (podBackend) for every decision. It keeps its own
// state under its own locks and knows no other module.
type podAPI struct {
	log   *slog.Logger
	clock *clock
	// wakeEnabled serves the wake socket; without it pods only poll.
	wakeEnabled bool
	backend     podBackend
	// forward relays a pod request to Multica under the route's rules.
	forward func(w http.ResponseWriter, r *http.Request, route *proxyRoute)

	// wakers are the open wake sockets by pod ID.
	wakeMu sync.Mutex
	wakers map[string]*wakeConn

	// modelReqs holds the model list request Multica has open for a runtime,
	// by runtime ID; see pool_models.go.
	modelMu   sync.Mutex
	modelReqs map[string]*modelRequest

	// notifiedAt is when a rate-limited workspaces_changed hint was last sent.
	notifyMu   sync.Mutex
	notifiedAt time.Time
}

// podBackend is what the Pod API asks the Dispatcher. Every method answers
// for the pod with an ID the Pod API resolved from the caller's pod token;
// none of them writes to the pod.
type podBackend interface {
	// notReady explains why the manager is not ready, "" when it is.
	notReady() string
	// sandboxPaths serves the sandbox service's own pod paths: the pod
	// registration and the supervisor's control channel.
	sandboxPaths() http.Handler
	// podByToken returns the ID of the pod holding the pod token.
	podByToken(token string) (string, bool)
	// servedWorkspaces lists the served workspaces for the daemon.
	servedWorkspaces() []workspaceRef
	// registerPod registers a pod's daemon for a workspace and returns the
	// registration answer.
	registerPod(ctx context.Context, podID string, reg podRegistration) (map[string]json.RawMessage, *podError)
	// heartbeatPod takes a pod daemon's heartbeat for a runtime.
	heartbeatPod(ctx context.Context, podID, runtimeID string) *podError
	// claimFor returns the task placed on the pod to run next, nil when none.
	claimFor(ctx context.Context, podID string) json.RawMessage
	// servedWorkspace returns the served workspace's canonical ID.
	servedWorkspace(id string) (string, bool)
	// reposRoute is the route for reading a workspace's repos, when the pod
	// may.
	reposRoute(podID, workspaceID string) (*proxyRoute, bool)
	// runtimeTicket is the bot account's ticket when a served workspace owns
	// the runtime.
	runtimeTicket(runtimeID string) (string, bool)
	// taskRoute is the route for a pod's report on a task, when the task is
	// the pod's.
	taskRoute(podID, taskID, action, path string) (*proxyRoute, bool)
}

// workspaceRef is a served workspace as the daemon lists it.
type workspaceRef struct {
	ID, Name string
}

// podRegistration is a pod daemon's registration as the Pod API decoded it.
type podRegistration struct {
	// raw is the body as the daemon sent it.
	raw         json.RawMessage
	WorkspaceID string
	DeviceName  string
	// offered are the agent CLIs the pod's daemon offers.
	offered map[string]bool
	// headers are the daemon's identity headers.
	headers map[string]string
}

// podError is a refusal the Pod API answers a pod with. A refused request is
// logged with its path.
type podError struct {
	status  int
	msg     string
	refused bool
}

func refused(why string) *podError {
	return &podError{status: http.StatusForbidden, msg: why, refused: true}
}

func newPodAPI(log *slog.Logger, clk *clock, wakeEnabled bool, backend podBackend, forward func(http.ResponseWriter, *http.Request, *proxyRoute)) *podAPI {
	return &podAPI{log: log, clock: clk, wakeEnabled: wakeEnabled, backend: backend, forward: forward, wakers: map[string]*wakeConn{}}
}

func (a *podAPI) answerError(w http.ResponseWriter, r *http.Request, podID string, e *podError) {
	if e.refused {
		a.refuse(w, r, podID, e.msg)
		return
	}
	http.Error(w, e.msg, e.status)
}

func (a *podAPI) refuse(w http.ResponseWriter, r *http.Request, podID, why string) {
	a.log.Warn("refused pod request", "pod_id", podID, "method", r.Method, "path", r.URL.Path, "reason", why)
	http.Error(w, why, http.StatusForbidden)
}

// handler serves the standard daemon protocol to pods, and passes the pod
// registration and the control channel to the sandbox service. Callers are
// identified by the pod token they present as their bearer token; the source
// address plays no part, since a mesh may rewrite it. An agent CLI's own task
// token ("mat_") is relayed upstream without identifying the pod.
func (a *podAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			// Liveness: the process answers. Readiness is /readyz.
			writeJSON(w, http.StatusOK, okBody)
			return
		case "/readyz":
			if why := a.backend.notReady(); why != "" {
				http.Error(w, why, http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, http.StatusOK, okBody)
			return
		case sandbox.RegisterPath, sandbox.ControlPath:
			a.backend.sandboxPaths().ServeHTTP(w, r)
			return
		}
		token := bearer(r)
		if strings.HasPrefix(token, "mat_") && isAgentPath(r.URL.Path) {
			// The task token authenticates itself upstream, so the request is
			// forwarded unchanged and never gets a workspace PAT.
			a.forward(w, r, &proxyRoute{})
			return
		}
		podID, ok := a.backend.podByToken(token)
		if !ok {
			a.log.Warn("refused pod request: unknown pod token", "method", r.Method, "path", r.URL.Path)
			http.Error(w, unknownPod.msg, unknownPod.status)
			return
		}
		switch r.URL.Path {
		case "/api/workspaces", "/api/daemon/workspaces":
			order := a.backend.servedWorkspaces()
			list := make([]map[string]string, 0, len(order))
			for _, ws := range order {
				list = append(list, map[string]string{"id": ws.ID, "name": ws.Name})
			}
			writeJSON(w, http.StatusOK, list)
		case "/api/tokens/current/renew":
			writeJSON(w, http.StatusOK, map[string]any{"expires_at": "", "renewed": false})
		case "/api/daemon/register":
			a.serveRegister(w, r, podID)
		case "/api/daemon/heartbeat":
			a.serveHeartbeat(w, r, podID)
		default:
			a.serveDaemon(w, r, podID)
		}
	})
}

// bearer is the request's bearer token, "" when it has none.
func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(token)
}

// isAgentPath reports whether path is one an agent's multica CLI may reach
// with its task token: anything but the daemon protocol, the pod's own calls
// and the paths the manager answers for the daemon itself.
func isAgentPath(path string) bool {
	switch path {
	case "/api/workspaces", "/api/tokens/current/renew", sandbox.ControlPath, sandbox.RegisterPath:
		return false
	}
	return !strings.HasPrefix(path, "/api/daemon/")
}

func (a *podAPI) serveRegister(w http.ResponseWriter, r *http.Request, podID string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		DeviceName  string `json:"device_name"`
		Runtimes    []struct {
			Type string `json:"type"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	headers := map[string]string{}
	for _, h := range []string{"X-Client-Platform", "X-Client-Version", "X-Client-OS", "X-Client-Capabilities"} {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	offered := map[string]bool{}
	for _, rt := range req.Runtimes {
		offered[rt.Type] = true
	}
	resp, perr := a.backend.registerPod(r.Context(), podID, podRegistration{
		raw: raw, WorkspaceID: req.WorkspaceID, DeviceName: req.DeviceName,
		offered: offered, headers: headers,
	})
	if perr != nil {
		a.answerError(w, r, podID, perr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *podAPI) serveHeartbeat(w http.ResponseWriter, r *http.Request, podID string) {
	var req struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if perr := a.backend.heartbeatPod(r.Context(), podID, req.RuntimeID); perr != nil {
		a.answerError(w, r, podID, perr)
		return
	}
	answer := map[string]any{"runtime_id": req.RuntimeID, "status": "ok"}
	if id := a.takeModelRequest(req.RuntimeID, podID); id != "" {
		answer["pending_model_list"] = daemonwire.DaemonHeartbeatPendingModelList{ID: id}
	}
	writeJSON(w, http.StatusOK, answer)
}

// serveDaemon answers the daemon endpoints beyond registration and heartbeat.
// Endpoints not listed are refused and logged, so a newer daemon's protocol
// additions are noticed instead of silently misrouted.
func (a *podAPI) serveDaemon(w http.ResponseWriter, r *http.Request, podID string) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/daemon/")
	if !ok {
		a.serveAgentAPI(w, r, podID)
		return
	}
	parts := strings.Split(rest, "/")
	switch {
	case rest == "deregister":
		// The manager owns the upstream runtime's lifetime.
		writeJSON(w, http.StatusOK, okBody)
	case rest == "ws":
		// The wake socket carries task hints only; claims stay on HTTP.
		a.serveWake(w, r, podID)
	case rest == "tasks/claim":
		task := a.backend.claimFor(r.Context(), podID)
		tasks := []json.RawMessage{}
		if task != nil {
			tasks = append(tasks, task)
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
	case len(parts) == 3 && parts[0] == "workspaces" && (parts[2] == "repos" || parts[2] == "runtime-profiles"):
		wsID, ok := a.backend.servedWorkspace(parts[1])
		if !ok {
			a.refuse(w, r, podID, "workspace is not served by this pool manager")
			return
		}
		if parts[2] == "runtime-profiles" {
			// Custom profiles launch arbitrary commands; pods only run the
			// image's built-in providers.
			writeJSON(w, http.StatusOK, map[string]any{"workspace_id": wsID, "runtime_profiles": []any{}})
			return
		}
		// The repo list and settings are read with the manager's PAT, which
		// sees every workspace; an agent in a pod can call this too. Only the
		// pod working for the workspace may read them.
		route, ok := a.backend.reposRoute(podID, wsID)
		if !ok {
			a.refuse(w, r, podID, "workspace repos are served only to a pod working for the workspace")
			return
		}
		a.forward(w, r, route)
	case len(parts) >= 3 && parts[0] == "tasks":
		a.serveTask(w, r, podID, parts[1], parts[2])
	case len(parts) >= 2 && parts[0] == "runtimes":
		a.serveRuntimeScoped(w, r, podID, parts)
	default:
		a.refuse(w, r, podID, "daemon endpoint not supported by the pool manager")
	}
}

// serveAgentAPI answers a pod's call outside the daemon protocol. An agent's
// multica CLI reaches the manager with its own task token, which the handler
// relays before any pod is identified; a pod token proves nothing there, so
// the call is refused.
func (a *podAPI) serveAgentAPI(w http.ResponseWriter, r *http.Request, podID string) {
	a.refuse(w, r, podID, "endpoint not supported by the pool manager")
}

func (a *podAPI) serveRuntimeScoped(w http.ResponseWriter, r *http.Request, podID string, parts []string) {
	ticket, ok := a.backend.runtimeTicket(parts[1])
	if !ok {
		http.Error(w, "runtime not found", http.StatusNotFound)
		return
	}
	rest := parts[2:]
	switch {
	case len(rest) == 3 && rest[0] == "models" && rest[2] == "result":
		// The report of a model list request the manager handed to this pod.
		if !a.modelRequestHeldBy(parts[1], rest[1], podID) {
			a.refuse(w, r, podID, "model list request is not assigned to this pod")
			return
		}
		a.forward(w, r, &proxyRoute{managerAuth: true, ticket: ticket, after: func(status int) {
			if status < http.StatusInternalServerError {
				a.finishModelRequest(parts[1], rest[1])
			}
		}})
	case len(rest) == 1 && rest[0] == "recover-orphans":
		// Upstream this would fail every other pod's tasks on the shared
		// runtime; the manager handles lost pods itself.
		writeJSON(w, http.StatusOK, okBody)
	case len(rest) >= 3 && rest[0] == "tasks":
		a.serveTask(w, r, podID, rest[1], rest[2])
	case len(rest) >= 3 && (rest[0] == "update" || rest[0] == "local-skills"):
		// The manager answers these itself and never relays them.
		writeJSON(w, http.StatusOK, okBody)
	default:
		a.refuse(w, r, podID, "daemon endpoint not supported by the pool manager")
	}
}

// serveTask forwards a pod's report on a task to Multica, provided the task is
// leased to that pod.
func (a *podAPI) serveTask(w http.ResponseWriter, r *http.Request, podID, taskID, action string) {
	route, ok := a.backend.taskRoute(podID, taskID, action, r.URL.Path)
	if !ok {
		a.refuse(w, r, podID, "task is not assigned to this pod")
		return
	}
	a.forward(w, r, route)
}
