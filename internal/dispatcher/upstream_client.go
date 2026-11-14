package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"sandbox-manager/internal/daemonwire"
)

// upstreamClient is the Upstream module: everything the manager says to the
// Multica server goes through it. It knows nothing of pods, leases or
// workspaces beyond the connection and IDs a call needs; the Dispatcher
// decides what to call and acts on the answers.
//
// It keeps two pieces of state under its own lock: the daemon identity headers
// taken from the first pod (pods are homogeneous) and replayed on every call,
// and when each runtime was last heartbeated upstream.
type upstreamClient struct {
	base string
	// ticket is the account PAT the manager lists its workspaces with.
	ticket string
	log    *slog.Logger
	proxy  *httputil.ReverseProxy

	mu      sync.Mutex
	headers map[string]string
	beatAt  map[string]time.Time
	// daemonID is the manager's one daemon identity in every workspace: the
	// configured one, or derived on first use and kept for the process.
	daemonID string
}

func newUpstreamClient(base, ticket, daemonID string, log *slog.Logger) *upstreamClient {
	u := &upstreamClient{base: base, ticket: ticket, daemonID: daemonID, log: log}
	u.proxy = u.newProxy()
	return u
}

// identity returns the daemon_id the manager registers and claims under in
// every workspace. Unless configured, it is derived from the server URL and
// the account the PAT belongs to, so it survives restarts and PAT rotation and
// tells two accounts on one server apart.
func (u *upstreamClient) identity(ctx context.Context) (string, error) {
	u.mu.Lock()
	id := u.daemonID
	u.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := u.connect(u.ticket).call(ctx, http.MethodGet, "/api/me", nil, &me); err != nil {
		return "", fmt.Errorf("look up the account: %w", err)
	}
	if me.ID == "" {
		return "", errors.New("look up the account: no account ID in the answer")
	}
	sum := sha256.Sum256([]byte(u.base + "\x00" + me.ID))
	id = "sandbox-manager-" + hex.EncodeToString(sum[:])[:24]
	u.mu.Lock()
	u.daemonID = id
	u.mu.Unlock()
	return id, nil
}

// knownIdentity is the daemon_id identity resolved, or "" before the first
// Sync resolved it. Every served workspace was added by a Sync, so a workspace
// never sees "".
func (u *upstreamClient) knownIdentity() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.daemonID
}

// connect opens a workspace's connection, which acts with that workspace's
// credential.
func (u *upstreamClient) connect(ticket string) *upstream {
	return newUpstream(u.base, ticket, u.identityHeaders)
}

func (u *upstreamClient) identityHeaders() map[string]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers
}

// adoptHeaders keeps the first pod's identity headers; later ones are ignored.
func (u *upstreamClient) adoptHeaders(headers map[string]string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.headers == nil {
		u.headers = headers
	}
}

// listWorkspaces asks Multica which workspaces the account belongs to and
// returns their IDs.
func (u *upstreamClient) listWorkspaces(ctx context.Context) ([]listedWorkspace, error) {
	up := u.connect(u.ticket)
	var list []listedWorkspace
	err := up.call(ctx, http.MethodGet, "/api/daemon/workspaces", nil, &list)
	if statusOf(err) == http.StatusNotFound {
		err = up.call(ctx, http.MethodGet, "/api/workspaces", nil, &list) // an older server
	}
	if err != nil {
		return nil, err
	}
	return list, nil
}

// listedWorkspace is a workspace the bot account belongs to. The daemon
// listing carries no slug; the member listing does.
type listedWorkspace struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// workspaceSlugs reads the member listing for the slugs of the account's
// workspaces, by workspace ID.
func (u *upstreamClient) workspaceSlugs(ctx context.Context) (map[string]string, error) {
	var list []listedWorkspace
	if err := u.connect(u.ticket).call(ctx, http.MethodGet, "/api/workspaces", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, w := range list {
		out[strings.ToLower(w.ID)] = w.Slug
	}
	return out, nil
}

// register registers the manager's runtimes in a workspace and returns the
// stored answer and the runtimes in it.
func (u *upstreamClient) register(ctx context.Context, conn *upstream, body map[string]any) (json.RawMessage, []runtimeRef, error) {
	var raw json.RawMessage
	if err := conn.call(ctx, http.MethodPost, "/api/daemon/register", body, &raw); err != nil {
		return nil, nil, fmt.Errorf("register runtime: %w", err)
	}
	var resp struct {
		Runtimes []runtimeRef `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, fmt.Errorf("decode registration: %w", err)
	}
	if len(resp.Runtimes) == 0 {
		return nil, nil, errors.New("registration returned no runtimes")
	}
	return raw, resp.Runtimes, nil
}

// recoverOrphans fails whatever Multica still shows running on the runtime.
func (u *upstreamClient) recoverOrphans(ctx context.Context, conn *upstream, runtimeID string) error {
	return conn.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/recover-orphans", map[string]any{}, nil)
}

// upstreamHeartbeatEvery is how often a runtime is heartbeated upstream, however
// many pods heartbeat the manager. Multica marks a runtime offline after a few
// minutes of silence and stops handing out its tasks.
const upstreamHeartbeatEvery = 10 * time.Second

// heartbeatDue reports whether the runtime is due an upstream heartbeat at now,
// and if so counts it as sent.
func (u *upstreamClient) heartbeatDue(runtimeID string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.beatAt == nil {
		u.beatAt = map[string]time.Time{}
	}
	if now.Sub(u.beatAt[runtimeID]) < upstreamHeartbeatEvery {
		return false
	}
	u.beatAt[runtimeID] = now
	return true
}

// heartbeat heartbeats one runtime upstream.
func (u *upstreamClient) heartbeat(ctx context.Context, conn *upstream, runtimeID string) (daemonwire.DaemonHeartbeatAckPayload, error) {
	var ack daemonwire.DaemonHeartbeatAckPayload
	err := conn.call(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": runtimeID}, &ack)
	return ack, err
}

// claim claims at most one task from the given runtimes. A nil result with a
// nil error means nothing is queued.
func (u *upstreamClient) claim(ctx context.Context, conn *upstream, daemonID string, runtimeIDs []string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamClaimTimeout)
	defer cancel()
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	body := map[string]any{"daemon_id": daemonID, "runtime_ids": runtimeIDs, "max_tasks": 1}
	if err := conn.call(ctx, http.MethodPost, "/api/daemon/tasks/claim", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Tasks) == 0 {
		return nil, nil
	}
	// max_tasks is 1; anything beyond that would be an upstream bug, and the
	// server reclaims what is never started.
	return resp.Tasks[0], nil
}

// pendingTask is a task on a runtime's pending list.
type pendingTask struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"created_at"`
}

// pending lists a runtime's queued and dispatched tasks.
func (u *upstreamClient) pending(ctx context.Context, conn *upstream, runtimeID string) ([]pendingTask, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
	defer cancel()
	var list []pendingTask
	err := conn.call(ctx, http.MethodGet, "/api/daemon/runtimes/"+url.PathEscape(runtimeID)+"/tasks/pending", nil, &list)
	return list, err
}

// fail makes one fail call, bounded and detached from ctx's cancellation.
func (u *upstreamClient) fail(ctx context.Context, conn *upstream, taskID, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamCallTimeout)
	defer cancel()
	return conn.call(ctx, http.MethodPost, "/api/daemon/tasks/"+url.PathEscape(taskID)+"/fail", map[string]any{"error": reason}, nil)
}

// taskStatus asks Multica for a task's status with the endpoint the stock
// daemon polls while it runs a task.
func (u *upstreamClient) taskStatus(ctx context.Context, conn *upstream, taskID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
	defer cancel()
	var resp struct {
		Status string `json:"status"`
	}
	if err := conn.call(ctx, http.MethodGet, "/api/daemon/tasks/"+url.PathEscape(taskID)+"/status", nil, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}

// listedRuntime is a runtime as Multica lists it.
type listedRuntime struct {
	ID         string `json:"id"`
	DaemonID   string `json:"daemon_id"`
	Visibility string `json:"visibility"`
}

// listRuntimes lists the workspace's runtimes.
func (u *upstreamClient) listRuntimes(ctx context.Context, conn *upstream, workspaceID string) ([]listedRuntime, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
	defer cancel()
	var list []listedRuntime
	err := conn.callWith(ctx, map[string]string{"X-Workspace-ID": workspaceID}, http.MethodGet, "/api/runtimes", nil, &list)
	return list, err
}

// setVisibility sets one runtime's visibility.
func (u *upstreamClient) setVisibility(ctx context.Context, conn *upstream, workspaceID, runtimeID, visibility string) error {
	ctx, cancel := context.WithTimeout(ctx, upstreamCallTimeout)
	defer cancel()
	body := json.RawMessage(fmt.Sprintf(`{"visibility":%q}`, visibility))
	return conn.callWith(ctx, map[string]string{"X-Workspace-ID": workspaceID}, http.MethodPatch, "/api/runtimes/"+runtimeID, body, nil)
}

// forward relays a pod request to Multica under the route's rules.
func (u *upstreamClient) forward(w http.ResponseWriter, r *http.Request, route *proxyRoute) {
	u.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyContextKey{}, route)))
}

func (u *upstreamClient) newProxy() *httputil.ReverseProxy {
	target, _ := url.Parse(u.base)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if route, _ := pr.In.Context().Value(proxyContextKey{}).(*proxyRoute); route != nil && route.managerAuth {
				pr.Out.Header.Set("Authorization", "Bearer "+route.ticket)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if route, _ := resp.Request.Context().Value(proxyContextKey{}).(*proxyRoute); route != nil && route.after != nil {
				route.after(resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			u.log.Warn("forward pod request", "path", r.URL.Path, "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
}
