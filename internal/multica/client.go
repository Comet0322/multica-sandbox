package multica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client is the manager's account on the Multica server: everything the
// manager says to Multica goes through it or through a Conn it opened. It
// knows nothing of pods, leases or the lending policy; the manager decides
// what to call and acts on the answers.
//
// It keeps three pieces of state under its own lock: the daemon identity the
// manager registers under, the daemon identity headers taken from the first
// pod (pods are homogeneous) and replayed on every call, and when each
// runtime was last heartbeated.
type Client struct {
	base string
	// pat is the bot account's PAT, the same in every workspace.
	pat   string
	log   *slog.Logger
	proxy *httputil.ReverseProxy

	mu      sync.Mutex
	headers map[string]string
	beatAt  map[string]time.Time
	// daemonID is the manager's one daemon identity in every workspace: the
	// configured one, or derived on first use and kept for the process.
	daemonID string
}

// CheckBaseURL refuses a server URL that is not a plain http(s) base URL.
func CheckBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an http(s) base URL")
	}
	return nil
}

// NewClient returns the client of the account the PAT belongs to on the
// server at base. An empty daemonID is derived on first use (see Identity).
func NewClient(base, pat, daemonID string, log *slog.Logger) *Client {
	c := &Client{base: base, pat: pat, daemonID: daemonID, log: log}
	c.proxy = c.newProxy()
	return c
}

// PAT is the account's PAT, which forwarded calls act with (see Route).
func (c *Client) PAT() string { return c.pat }

// Identity returns the daemon_id the manager registers and claims under in
// every workspace. Unless configured, it is derived from the server URL and
// the account the PAT belongs to, so it survives restarts and PAT rotation and
// tells two accounts on one server apart.
func (c *Client) Identity(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.daemonID
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := c.Connect().call(ctx, http.MethodGet, "/api/me", nil, &me); err != nil {
		return "", fmt.Errorf("look up the account: %w", err)
	}
	if me.ID == "" {
		return "", errors.New("look up the account: no account ID in the answer")
	}
	sum := sha256.Sum256([]byte(c.base + "\x00" + me.ID))
	id = "sandbox-manager-" + hex.EncodeToString(sum[:])[:24]
	c.mu.Lock()
	c.daemonID = id
	c.mu.Unlock()
	return id, nil
}

// KnownIdentity is the daemon_id identity resolved, or "" before the first
// listing resolved it. Every served workspace was added by a listing, so a
// workspace never sees "".
func (c *Client) KnownIdentity() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.daemonID
}

// LegacyDaemonID is the daemon_id the manager used to derive for each
// workspace, before it had one identity everywhere. Registration sends it as
// a legacy id so the server merges those runtime rows into the one
// identity's, and orphan recovery treats its runtimes as the manager's own.
func (c *Client) LegacyDaemonID(workspaceID string) string {
	sum := sha256.Sum256([]byte(c.base + "\x00" + workspaceID))
	// The prefix is the name the program registered under before it was
	// renamed; changing it would orphan those rows.
	return "sandbox-broker-" + hex.EncodeToString(sum[:])[:24]
}

// Connect opens a connection that acts with the account's PAT.
func (c *Client) Connect() *Conn {
	return newConn(c.base, c.pat, c.identityHeaders)
}

func (c *Client) identityHeaders() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headers
}

// AdoptHeaders keeps the first pod's identity headers; later ones are ignored.
func (c *Client) AdoptHeaders(headers map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.headers == nil {
		c.headers = headers
	}
}

// ListedWorkspace is a workspace the bot account belongs to. The daemon
// listing carries no slug; the member listing does.
type ListedWorkspace struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// ListWorkspaces asks Multica which workspaces the account belongs to.
func (c *Client) ListWorkspaces(ctx context.Context) ([]ListedWorkspace, error) {
	conn := c.Connect()
	var list []ListedWorkspace
	err := conn.call(ctx, http.MethodGet, "/api/daemon/workspaces", nil, &list)
	if StatusOf(err) == http.StatusNotFound {
		err = conn.call(ctx, http.MethodGet, "/api/workspaces", nil, &list) // an older server
	}
	if err != nil {
		return nil, err
	}
	return list, nil
}

// WorkspaceSlugs reads the member listing for the slugs of the account's
// workspaces, by workspace ID.
func (c *Client) WorkspaceSlugs(ctx context.Context) (map[string]string, error) {
	var list []ListedWorkspace
	if err := c.Connect().call(ctx, http.MethodGet, "/api/workspaces", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, w := range list {
		out[strings.ToLower(w.ID)] = w.Slug
	}
	return out, nil
}

// HeartbeatEvery is how often a runtime is heartbeated upstream, however
// many pods heartbeat the manager. Multica marks a runtime offline after a few
// minutes of silence and stops handing out its tasks.
const HeartbeatEvery = 10 * time.Second

// HeartbeatDue reports whether the runtime is due an upstream heartbeat at
// now, and if so counts it as sent.
func (c *Client) HeartbeatDue(runtimeID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.beatAt == nil {
		c.beatAt = map[string]time.Time{}
	}
	if now.Sub(c.beatAt[runtimeID]) < HeartbeatEvery {
		return false
	}
	c.beatAt[runtimeID] = now
	return true
}

type routeContextKey struct{}

// Route carries the forwarding decisions for one relayed pod request.
type Route struct {
	// AccountAuth replaces the pod's credential with the account's PAT.
	AccountAuth bool
	// After runs with the upstream status code of the forwarded call.
	After func(status int)
}

// Forward relays a pod request to Multica under the route's rules.
func (c *Client) Forward(w http.ResponseWriter, r *http.Request, route *Route) {
	c.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeContextKey{}, route)))
}

func (c *Client) newProxy() *httputil.ReverseProxy {
	target, _ := url.Parse(c.base)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if route, _ := pr.In.Context().Value(routeContextKey{}).(*Route); route != nil && route.AccountAuth {
				pr.Out.Header.Set("Authorization", "Bearer "+c.pat)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if route, _ := resp.Request.Context().Value(routeContextKey{}).(*Route); route != nil && route.After != nil {
				route.After(resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			c.log.Warn("forward pod request", "path", r.URL.Path, "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
}
