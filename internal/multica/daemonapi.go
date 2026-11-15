package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"sandbox-manager/internal/multica/daemonwire"
)

// DaemonAPI plays the Multica server toward the stock daemon in each pod: it
// decodes and answers the daemon's HTTP calls, holds its wake socket and the
// model list requests handed to it, and asks the manager (Backend) for every
// decision. It keeps its own state under its own locks.
type DaemonAPI struct {
	cfg DaemonAPIConfig

	// wakers are the open wake sockets by pod ID.
	wakeMu sync.Mutex
	wakers map[string]*wakeConn

	// modelReqs holds the model list request Multica has open for a runtime,
	// by runtime ID; see models.go.
	modelMu   sync.Mutex
	modelReqs map[string]*modelRequest

	// notifiedAt is when a rate-limited workspaces_changed hint was last sent.
	notifyMu   sync.Mutex
	notifiedAt time.Time
}

// DaemonAPIConfig configures a DaemonAPI.
type DaemonAPIConfig struct {
	Log *slog.Logger
	Now func() time.Time
	// Wake serves the wake socket; without it pods only poll.
	Wake    bool
	Backend Backend
	// Forward relays a pod request to Multica under the route's rules
	// (Client.Forward).
	Forward func(w http.ResponseWriter, r *http.Request, route *Route)
	// PodPaths are served on the same listener ahead of the daemon protocol,
	// before any pod is identified: the sandbox service's registration and
	// control channel. An agent's task token never reaches them.
	PodPaths map[string]http.Handler
}

// Backend is what the DaemonAPI asks the manager. Every method answers for
// the pod with an ID the DaemonAPI resolved from the caller's pod token; none
// of them writes to the pod.
type Backend interface {
	// NotReady explains why the manager is not ready, "" when it is.
	NotReady() string
	// PodByToken returns the ID of the pod holding the pod token.
	PodByToken(token string) (string, bool)
	// ServedWorkspaces lists the served workspaces for the daemon.
	ServedWorkspaces() []WorkspaceRef
	// RegisterPod registers a pod's daemon for a workspace and returns the
	// registration answer.
	RegisterPod(ctx context.Context, podID string, reg Registration) (map[string]json.RawMessage, *PodError)
	// HeartbeatPod takes a pod daemon's heartbeat for a runtime.
	HeartbeatPod(ctx context.Context, podID, runtimeID string) *PodError
	// ClaimFor returns the task placed on the pod to run next, nil when none.
	ClaimFor(ctx context.Context, podID string) json.RawMessage
	// ServedWorkspace returns the served workspace's canonical ID.
	ServedWorkspace(id string) (string, bool)
	// ReposRoute is the route for reading a workspace's repos, when the pod
	// may.
	ReposRoute(podID, workspaceID string) (*Route, bool)
	// RuntimeServed reports whether a served workspace owns the runtime.
	RuntimeServed(runtimeID string) bool
	// ModelsReported takes a pod's model list report once Multica took it.
	ModelsReported(runtimeID string, report []byte)
	// TaskRoute is the route for a pod's report on a task, when the task is
	// the pod's.
	TaskRoute(podID, taskID, action, path string) (*Route, bool)
}

// WorkspaceRef is a served workspace as the daemon lists it.
type WorkspaceRef struct {
	ID, Name string
}

// Registration is a pod daemon's registration as the DaemonAPI decoded it.
type Registration struct {
	// Raw is the body as the daemon sent it.
	Raw         json.RawMessage
	WorkspaceID string
	DeviceName  string
	// Offered are the agent CLIs the pod's daemon offers.
	Offered map[string]bool
	// Headers are the daemon's identity headers.
	Headers map[string]string
}

// PodError is a refusal the DaemonAPI answers a pod with.
type PodError struct {
	Status int
	Msg    string
	// Refused answers 403 and logs the request with its path.
	Refused bool
}

// Refuse is the refusal of a request the pod may not make.
func Refuse(why string) *PodError {
	return &PodError{Status: http.StatusForbidden, Msg: why, Refused: true}
}

// UnknownPod answers a call whose pod token names no pod (any more).
var UnknownPod = &PodError{Status: http.StatusUnauthorized, Msg: "unknown pod token"}

// NewDaemonAPI returns the daemon protocol server.
func NewDaemonAPI(cfg DaemonAPIConfig) *DaemonAPI {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &DaemonAPI{cfg: cfg, wakers: map[string]*wakeConn{}}
}

func (a *DaemonAPI) now() time.Time { return a.cfg.Now() }

func (a *DaemonAPI) answerError(w http.ResponseWriter, r *http.Request, podID string, e *PodError) {
	if e.Refused {
		a.refuse(w, r, podID, e.Msg)
		return
	}
	http.Error(w, e.Msg, e.Status)
}

func (a *DaemonAPI) refuse(w http.ResponseWriter, r *http.Request, podID, why string) {
	a.cfg.Log.Warn("refused pod request", "pod_id", podID, "method", r.Method, "path", r.URL.Path, "reason", why)
	http.Error(w, why, http.StatusForbidden)
}

// Handler serves the standard daemon protocol to pods, and the PodPaths
// beside it. Callers are identified by the pod token they present as their
// bearer token; the source address plays no part, since a mesh may rewrite
// it. An agent CLI's own task token ("mat_") is relayed upstream without
// identifying the pod.
func (a *DaemonAPI) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			// Liveness: the process answers. Readiness is /readyz.
			writeJSON(w, http.StatusOK, okBody)
			return
		case "/readyz":
			if why := a.cfg.Backend.NotReady(); why != "" {
				http.Error(w, why, http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, http.StatusOK, okBody)
			return
		}
		if h, ok := a.cfg.PodPaths[r.URL.Path]; ok {
			h.ServeHTTP(w, r)
			return
		}
		token := bearer(r)
		if strings.HasPrefix(token, "mat_") && a.isAgentPath(r.URL.Path) {
			// The task token authenticates itself upstream, so the request is
			// forwarded unchanged and never gets the account's PAT.
			a.cfg.Forward(w, r, &Route{})
			return
		}
		podID, ok := a.cfg.Backend.PodByToken(token)
		if !ok {
			a.cfg.Log.Warn("refused pod request: unknown pod token", "method", r.Method, "path", r.URL.Path)
			http.Error(w, UnknownPod.Msg, UnknownPod.Status)
			return
		}
		switch r.URL.Path {
		case "/api/workspaces", "/api/daemon/workspaces":
			order := a.cfg.Backend.ServedWorkspaces()
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
func (a *DaemonAPI) isAgentPath(path string) bool {
	if _, ok := a.cfg.PodPaths[path]; ok {
		return false
	}
	switch path {
	case "/api/workspaces", "/api/tokens/current/renew":
		return false
	}
	return !strings.HasPrefix(path, "/api/daemon/")
}

func (a *DaemonAPI) serveRegister(w http.ResponseWriter, r *http.Request, podID string) {
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
	resp, perr := a.cfg.Backend.RegisterPod(r.Context(), podID, Registration{
		Raw: raw, WorkspaceID: req.WorkspaceID, DeviceName: req.DeviceName,
		Offered: offered, Headers: headers,
	})
	if perr != nil {
		a.answerError(w, r, podID, perr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *DaemonAPI) serveHeartbeat(w http.ResponseWriter, r *http.Request, podID string) {
	var req struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if perr := a.cfg.Backend.HeartbeatPod(r.Context(), podID, req.RuntimeID); perr != nil {
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
func (a *DaemonAPI) serveDaemon(w http.ResponseWriter, r *http.Request, podID string) {
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
		task := a.cfg.Backend.ClaimFor(r.Context(), podID)
		tasks := []json.RawMessage{}
		if task != nil {
			tasks = append(tasks, task)
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
	case len(parts) == 3 && parts[0] == "workspaces" && (parts[2] == "repos" || parts[2] == "runtime-profiles"):
		wsID, ok := a.cfg.Backend.ServedWorkspace(parts[1])
		if !ok {
			a.refuse(w, r, podID, "workspace is not served by this sandbox manager")
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
		route, ok := a.cfg.Backend.ReposRoute(podID, wsID)
		if !ok {
			a.refuse(w, r, podID, "workspace repos are served only to a pod working for the workspace")
			return
		}
		a.cfg.Forward(w, r, route)
	case len(parts) >= 3 && parts[0] == "tasks":
		a.serveTask(w, r, podID, parts[1], parts[2])
	case len(parts) >= 2 && parts[0] == "runtimes":
		a.serveRuntimeScoped(w, r, podID, parts)
	default:
		a.refuse(w, r, podID, "daemon endpoint not supported by the sandbox manager")
	}
}

// serveAgentAPI answers a pod's call outside the daemon protocol. An agent's
// multica CLI reaches the manager with its own task token, which the handler
// relays before any pod is identified; a pod token proves nothing there, so
// the call is refused.
func (a *DaemonAPI) serveAgentAPI(w http.ResponseWriter, r *http.Request, podID string) {
	a.refuse(w, r, podID, "endpoint not supported by the sandbox manager")
}

func (a *DaemonAPI) serveRuntimeScoped(w http.ResponseWriter, r *http.Request, podID string, parts []string) {
	if !a.cfg.Backend.RuntimeServed(parts[1]) {
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
		report, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(report))
		a.cfg.Forward(w, r, &Route{AccountAuth: true, After: func(status int) {
			if status < http.StatusInternalServerError {
				a.finishModelRequest(parts[1], rest[1])
			}
			if status >= 200 && status <= 299 {
				a.cfg.Backend.ModelsReported(parts[1], report)
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
		a.refuse(w, r, podID, "daemon endpoint not supported by the sandbox manager")
	}
}

// serveTask forwards a pod's report on a task to Multica, provided the task is
// leased to that pod.
func (a *DaemonAPI) serveTask(w http.ResponseWriter, r *http.Request, podID, taskID, action string) {
	route, ok := a.cfg.Backend.TaskRoute(podID, taskID, action, r.URL.Path)
	if !ok {
		a.refuse(w, r, podID, "task is not assigned to this pod")
		return
	}
	a.cfg.Forward(w, r, route)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var okBody = map[string]string{"status": "ok"}
