package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sandbox-manager/internal/multica/daemonwire"
)

const (
	// ClaimTimeout bounds one claim call, so a hanging workspace delays the
	// other workspaces' claims by at most this long.
	ClaimTimeout = 10 * time.Second
	// CallTimeout bounds one pending, status, fail or runtime call.
	CallTimeout = 10 * time.Second
)

// Conn is one workspace's connection to the Multica server. It acts with the
// PAT it was opened with and replays the daemon identity headers the Client
// adopted, so one workspace failing cannot touch another.
type Conn struct {
	base, pat string
	client    *http.Client
	// headers returns the daemon identity headers to replay on every call.
	headers func() map[string]string
}

func newConn(base, pat string, headers func() map[string]string) *Conn {
	return &Conn{
		base: base, pat: pat, headers: headers,
		client: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Close drops the connection's idle connections, once its workspace is no
// longer served.
func (c *Conn) Close() { c.client.CloseIdleConnections() }

// Error is a call Multica answered with a status outside 2xx.
type Error struct {
	Status int
	Path   string
	// RuntimeNotFound is set on a 404 whose body says "runtime not found": the
	// runtime row is gone upstream (deleted in the UI, or collected after a
	// long time offline), as the stock daemon reads it.
	RuntimeNotFound bool
}

// Bodies are omitted: they may echo task content or credentials.
func (e *Error) Error() string {
	return fmt.Sprintf("Multica %s returned HTTP %d", e.Path, e.Status)
}

// StatusOf is the HTTP status Multica answered err with, 0 when err is not
// an answer from Multica (the network failed, say).
func StatusOf(err error) int {
	if e, ok := err.(*Error); ok {
		return e.Status
	}
	return 0
}

// IsRuntimeNotFound reports whether Multica answered that the runtime is gone.
func IsRuntimeNotFound(err error) bool {
	e, ok := err.(*Error)
	return ok && e.RuntimeNotFound
}

// RetryableFail reports whether a failed fail call is worth sending again: the
// network or Multica itself failed. Any other refusal (4xx) means Multica has
// dealt with the task already, or never will.
func RetryableFail(err error) bool {
	code := StatusOf(err)
	return code == 0 || code == http.StatusTooManyRequests || code >= 500
}

func (c *Conn) call(ctx context.Context, method, path string, in, out any) error {
	return c.callWith(ctx, nil, method, path, in, out)
}

// callWith is call with extra request headers.
func (c *Conn) callWith(ctx context.Context, extra map[string]string, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, ok := in.(json.RawMessage)
		if !ok {
			var err error
			if raw, err = json.Marshal(in); err != nil {
				return err
			}
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("Multica %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode, Path: path}
		if resp.StatusCode == http.StatusNotFound {
			// Only this marker is kept; the body itself is never stored.
			head, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			e.RuntimeNotFound = strings.Contains(strings.ToLower(string(head)), "runtime not found")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return e
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return err
		}
		*raw = data
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

// Register registers runtimes in the connection's workspace and returns the
// stored answer and the runtimes in it.
func (c *Conn) Register(ctx context.Context, body map[string]any) (json.RawMessage, []RuntimeRef, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodPost, "/api/daemon/register", body, &raw); err != nil {
		return nil, nil, fmt.Errorf("register runtime: %w", err)
	}
	runtimes := RuntimesOf(raw)
	if runtimes == nil {
		return nil, nil, errors.New("registration returned no runtimes")
	}
	return raw, runtimes, nil
}

// RecoverOrphans fails whatever Multica still shows running on the runtime.
func (c *Conn) RecoverOrphans(ctx context.Context, runtimeID string) error {
	return c.call(ctx, http.MethodPost, "/api/daemon/runtimes/"+url.PathEscape(runtimeID)+"/recover-orphans", map[string]any{}, nil)
}

// Heartbeat heartbeats one runtime.
func (c *Conn) Heartbeat(ctx context.Context, runtimeID string) (daemonwire.DaemonHeartbeatAckPayload, error) {
	var ack daemonwire.DaemonHeartbeatAckPayload
	err := c.call(ctx, http.MethodPost, "/api/daemon/heartbeat", map[string]any{"runtime_id": runtimeID}, &ack)
	return ack, err
}

// Claim claims at most one task from the given runtimes. A nil result with a
// nil error means nothing is queued.
func (c *Conn) Claim(ctx context.Context, daemonID string, runtimeIDs []string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, ClaimTimeout)
	defer cancel()
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	body := map[string]any{"daemon_id": daemonID, "runtime_ids": runtimeIDs, "max_tasks": 1}
	if err := c.call(ctx, http.MethodPost, "/api/daemon/tasks/claim", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Tasks) == 0 {
		return nil, nil
	}
	// max_tasks is 1; anything beyond that would be an upstream bug, and the
	// server reclaims what is never started.
	return resp.Tasks[0], nil
}

// Pending lists a runtime's queued and dispatched tasks.
func (c *Conn) Pending(ctx context.Context, runtimeID string) ([]PendingTask, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var list []PendingTask
	err := c.call(ctx, http.MethodGet, "/api/daemon/runtimes/"+url.PathEscape(runtimeID)+"/tasks/pending", nil, &list)
	return list, err
}

// Fail makes one fail call, bounded and detached from ctx's cancellation.
func (c *Conn) Fail(ctx context.Context, taskID, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CallTimeout)
	defer cancel()
	return c.call(ctx, http.MethodPost, "/api/daemon/tasks/"+url.PathEscape(taskID)+"/fail", map[string]any{"error": reason}, nil)
}

// TaskStatus asks Multica for a task's status with the endpoint the stock
// daemon polls while it runs a task.
func (c *Conn) TaskStatus(ctx context.Context, taskID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var resp struct {
		Status string `json:"status"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/daemon/tasks/"+url.PathEscape(taskID)+"/status", nil, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}

// ListedRuntime is a runtime as Multica lists it.
type ListedRuntime struct {
	ID         string `json:"id"`
	DaemonID   string `json:"daemon_id"`
	Visibility string `json:"visibility"`
}

// ListRuntimes lists the workspace's runtimes.
func (c *Conn) ListRuntimes(ctx context.Context, workspaceID string) ([]ListedRuntime, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var list []ListedRuntime
	err := c.callWith(ctx, map[string]string{"X-Workspace-ID": workspaceID}, http.MethodGet, "/api/runtimes", nil, &list)
	return list, err
}

// SetVisibility sets one runtime's visibility.
func (c *Conn) SetVisibility(ctx context.Context, workspaceID, runtimeID, visibility string) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	body := json.RawMessage(fmt.Sprintf(`{"visibility":%q}`, visibility))
	return c.callWith(ctx, map[string]string{"X-Workspace-ID": workspaceID}, http.MethodPatch, "/api/runtimes/"+url.PathEscape(runtimeID), body, nil)
}
