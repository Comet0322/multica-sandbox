package multica

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Runtimes is the manager's runtime registration in one workspace: the
// runtimes it registered there on the first pod's behalf, which of them
// Multica said are gone, whether they were seen with the wanted visibility,
// and how the latest call there went. The pods talk to the manager, not to
// Multica, so these runtimes stand for every pod at once.
//
// It keeps that state under its own lock and calls nothing back while holding
// it, so the manager may call it under its own lock.
type Runtimes struct {
	cfg    RuntimesConfig
	client *Client
	conn   *Conn

	// registerMu serializes the workspace's registration.
	registerMu sync.Mutex

	mu           sync.Mutex
	registration json.RawMessage
	// gone are runtime IDs Multica said no longer exist; a pod that
	// heartbeats one is told so, which makes its daemon register again.
	gone map[string]bool
	// shared is set once the registered runtimes were seen with the wanted
	// visibility; until then every EnsureVisibility tries again.
	shared bool
	// health is the outcome of the latest claim, pending read or heartbeat.
	health *Health
}

// RuntimesConfig configures one workspace's Runtimes.
type RuntimesConfig struct {
	WorkspaceID string
	// Name is the machine name members see in the workspace's runtime list.
	Name string
	// DeviceName is shown under it; empty shows Name.
	DeviceName string
	// DaemonIDSuffix follows the manager's identity in the daemon_id these
	// runtimes are registered and claimed under, so each runtime kind is a
	// machine of its own (ADR 0009). Empty is the identity itself, which
	// alone takes over the legacy daemon_id's runtimes.
	DaemonIDSuffix string
	// Providers limits which agent CLIs a pod reports become runtimes. Empty
	// registers every reported provider.
	Providers []string
	// Visibility is set on every registered runtime: "public" lets every
	// member of the workspace use it, "private" leaves it to its owner.
	Visibility string
	Log        *slog.Logger
	Now        func() time.Time
}

// Health is the outcome of a workspace's latest call to Multica.
type Health struct {
	OK bool `json:"ok"`
	// Op is the call: "claim", "pending" or "heartbeat".
	Op    string    `json:"op"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// Runtimes opens the workspace's connection and returns its registration,
// empty until a pod registers the workspace.
func (c *Client) Runtimes(cfg RuntimesConfig) *Runtimes {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runtimes{cfg: cfg, client: c, conn: c.Connect()}
}

// daemonID is the daemon_id these runtimes are registered and claimed under.
func (r *Runtimes) daemonID() string { return r.client.KnownIdentity() + r.cfg.DaemonIDSuffix }

// legacyID is the daemon_id these runtimes were registered under before,
// empty when none.
func (r *Runtimes) legacyID() string {
	if r.cfg.DaemonIDSuffix != "" {
		return ""
	}
	return r.client.LegacyDaemonID(r.cfg.WorkspaceID)
}

// Conn is the workspace's connection.
func (r *Runtimes) Conn() *Conn { return r.conn }

// IDs lists the registered runtime IDs, none before the first registration.
func (r *Runtimes) IDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RuntimeIDs(r.registration)
}

// Provider returns the agent CLI the runtime was registered for, "" when it
// is not one of the registered ones.
func (r *Runtimes) Provider(runtimeID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rt := range RuntimesOf(r.registration) {
		if rt.ID == runtimeID {
			return rt.Provider
		}
	}
	return ""
}

// Registered reports whether the workspace has a registration.
func (r *Runtimes) Registered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.registration) > 0
}

// Owns reports whether the runtime is one of the registered ones.
func (r *Runtimes) Owns(runtimeID string) bool {
	return slices.Contains(r.IDs(), runtimeID)
}

// Gone reports whether Multica said the runtime no longer exists.
func (r *Runtimes) Gone(runtimeID string) bool {
	if runtimeID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gone[runtimeID]
}

// Health copies the outcome of the latest call; nil before the first.
func (r *Runtimes) Health() *Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.health == nil {
		return nil
	}
	h := *r.health
	return &h
}

// RecordHealth keeps the outcome of a call for the inventory. The error text
// is the upstream error's, which carries no response body.
func (r *Runtimes) RecordHealth(op string, err error) {
	h := &Health{OK: err == nil, Op: op, At: r.cfg.Now()}
	if err != nil {
		h.Error = boundText(err.Error(), 300)
	}
	r.mu.Lock()
	r.health = h
	r.mu.Unlock()
}

// Register registers the manager's runtimes from a pod daemon's
// registration, the first time and whenever a pod offers an agent CLI the
// registration has no runtime for, and returns the stored answer. busy
// reports whether the workspace still has work in the manager; it is asked,
// without any lock of Runtimes held, before a first registration recovers
// orphaned tasks.
func (r *Runtimes) Register(ctx context.Context, captured json.RawMessage, headers map[string]string, busy func() bool) (json.RawMessage, error) {
	r.registerMu.Lock()
	defer r.registerMu.Unlock()
	r.mu.Lock()
	registration := r.registration
	r.mu.Unlock()
	body, err := r.body(captured)
	if len(registration) > 0 {
		if err != nil || CoversProviders(registration, body) {
			return registration, nil
		}
		return r.registerMore(ctx, body)
	}
	if err != nil {
		return nil, err
	}
	r.client.AdoptHeaders(headers)
	raw, runtimes, err := r.conn.Register(ctx, body)
	if err != nil {
		return nil, err
	}
	// What follows must not die with the pod's request that triggered it.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if busy() {
		// The workspace was registered before and still has work here (its
		// runtime was re-created upstream, or it came back after an absence):
		// the tasks on these runtimes are live, not orphans.
		r.cfg.Log.Info("skipped orphan recovery: the workspace has active leases", "workspace_id", r.cfg.WorkspaceID)
	} else {
		// Whatever Multica still shows running on these runtimes belongs to
		// no lease here and will never finish (the manager's first listing did
		// the same for the runtimes that existed then; these may be new). The
		// workspace holds no lease or claimed task in this manager, and none
		// can be claimed until the registration below is stored, so failing
		// them (which lets Multica retry) cannot hit a live task. Best effort:
		// the tasks would otherwise wait for Multica's own timeout.
		for _, rt := range runtimes {
			if err := r.conn.RecoverOrphans(bg, rt.ID); err != nil {
				r.cfg.Log.Warn("recover orphaned tasks upstream", "workspace_id", r.cfg.WorkspaceID, "runtime_id", rt.ID, "error", err)
			}
		}
	}
	r.store(raw, runtimes)
	r.cfg.Log.Info("registered the manager's runtimes", "workspace_id", r.cfg.WorkspaceID, "runtimes", len(runtimes))
	r.share(bg, runtimes)
	return raw, nil
}

// registerMore registers again when a pod offers agent CLIs the stored
// registration has no runtime for (the image gained one while the manager
// kept running). Registering the same machine again only adds the new
// runtimes. The workspace's tasks and keys are untouched.
func (r *Runtimes) registerMore(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	raw, runtimes, err := r.conn.Register(ctx, body)
	if err != nil {
		return nil, err
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	r.store(raw, runtimes)
	r.cfg.Log.Info("registered more of the manager's runtimes", "workspace_id", r.cfg.WorkspaceID, "runtimes", len(runtimes))
	r.share(bg, runtimes)
	return raw, nil
}

func (r *Runtimes) store(raw json.RawMessage, runtimes []RuntimeRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registration = raw
	r.shared = false
	for _, rt := range runtimes {
		delete(r.gone, rt.ID)
	}
}

// body rewrites a pod daemon's registration into the manager's: one machine
// identity, local mode, no custom profiles.
func (r *Runtimes) body(captured json.RawMessage) (map[string]any, error) {
	id := Identity{
		WorkspaceID: r.cfg.WorkspaceID, DaemonID: r.daemonID(), Name: r.cfg.Name, DeviceName: r.cfg.DeviceName,
		Providers: r.cfg.Providers, Source: "the pod", Mode: RuntimeMode,
	}
	if old := r.legacyID(); old != "" && old != id.DaemonID {
		id.LegacyIDs = []string{old}
	}
	return RewriteRegistration(captured, id)
}

// Drop forgets the registration after Multica answered that one of its
// runtimes is gone (deleted in the UI, or collected after a long time
// offline), and reports whether it did. The next pod registration for the
// workspace registers it again; pods heartbeating the gone runtime are told
// so, which is what makes the stock daemon register again.
func (r *Runtimes) Drop(runtimeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(RuntimeIDs(r.registration), runtimeID) {
		return false // already dropped, or registered again since
	}
	if r.gone == nil {
		r.gone = map[string]bool{}
	}
	r.gone[runtimeID] = true
	r.registration = nil
	r.shared = false
	return true
}

// Heartbeat heartbeats one runtime upstream and records the outcome. It
// returns the model list request Multica handed out with the answer, "" when
// none.
func (r *Runtimes) Heartbeat(ctx context.Context, runtimeID string) (string, error) {
	ack, err := r.conn.Heartbeat(ctx, runtimeID)
	r.RecordHealth("heartbeat", err)
	if err != nil || ack.PendingModelList == nil {
		return "", err
	}
	return ack.PendingModelList.ID, nil
}

// Claim claims at most one task from the registered runtimes and records
// the outcome. A nil result with a nil error means nothing is queued.
func (r *Runtimes) Claim(ctx context.Context, runtimeIDs []string) (json.RawMessage, error) {
	raw, err := r.conn.Claim(ctx, r.daemonID(), runtimeIDs)
	r.RecordHealth("claim", err)
	return raw, err
}

// Pending reads the pending lists of the given runtimes, merged. A failed
// read is recorded.
func (r *Runtimes) Pending(ctx context.Context, runtimeIDs []string) ([]PendingTask, error) {
	var list []PendingTask
	for _, id := range runtimeIDs {
		tasks, err := r.conn.Pending(ctx, id)
		if err != nil {
			r.RecordHealth("pending", err)
			return nil, err
		}
		list = append(list, tasks...)
	}
	return list, nil
}

// RecoverOrphans fails whatever Multica still shows running on the
// manager's runtimes in the workspace, those of its identity and its legacy
// one. The caller makes sure the workspace has no live work here. Best
// effort: a failure leaves the tasks to Multica's own timeout.
func (r *Runtimes) RecoverOrphans(ctx context.Context) {
	runtimes, err := r.conn.ListRuntimes(ctx, r.cfg.WorkspaceID)
	if err != nil {
		r.cfg.Log.Warn("list runtimes to recover orphaned tasks", "workspace_id", r.cfg.WorkspaceID, "error", err)
		return
	}
	own, legacy := r.daemonID(), r.legacyID()
	for _, rt := range runtimes {
		if rt.DaemonID != own && (legacy == "" || rt.DaemonID != legacy) {
			continue
		}
		if err := r.conn.RecoverOrphans(ctx, rt.ID); err != nil {
			r.cfg.Log.Warn("recover orphaned tasks upstream", "workspace_id", r.cfg.WorkspaceID, "runtime_id", rt.ID, "error", err)
		}
	}
}

// EnsureVisibility makes sure the registered runtimes have the wanted
// visibility, for a registration whose share call failed. Once they were
// seen right they are left alone, so an owner who changes the visibility by
// hand afterwards keeps that choice. Failures are logged and retried on the
// next call.
func (r *Runtimes) EnsureVisibility(ctx context.Context) {
	if r.cfg.Visibility != "public" {
		return
	}
	r.mu.Lock()
	todo := !r.shared && len(r.registration) > 0
	ids := RuntimeIDs(r.registration)
	r.mu.Unlock()
	if !todo {
		return
	}
	list, err := r.conn.ListRuntimes(ctx, r.cfg.WorkspaceID)
	if err != nil {
		r.cfg.Log.Warn("check runtime visibility", "workspace_id", r.cfg.WorkspaceID, "error", err)
		return
	}
	var wrong []RuntimeRef
	for _, rt := range list {
		if slices.Contains(ids, rt.ID) && rt.Visibility != r.cfg.Visibility {
			wrong = append(wrong, RuntimeRef{ID: rt.ID})
		}
	}
	if len(wrong) == 0 {
		r.mu.Lock()
		if slices.Equal(RuntimeIDs(r.registration), ids) {
			r.shared = true
		}
		r.mu.Unlock()
		return
	}
	r.cfg.Log.Info("runtime not shared yet; sharing again", "workspace_id", r.cfg.WorkspaceID, "runtimes", len(wrong))
	r.share(ctx, wrong)
}

// share sets the visibility of freshly registered runtimes. A runtime belongs
// to the account the manager runs as and starts out private, so without this
// only that account could bind agents to it. Only the owner may change it,
// which is why the manager does it with the same PAT. Best effort:
// EnsureVisibility retries until the runtimes are seen with the wanted
// visibility.
func (r *Runtimes) share(ctx context.Context, runtimes []RuntimeRef) {
	if r.cfg.Visibility != "public" {
		return
	}
	for _, rt := range runtimes {
		if err := r.conn.SetVisibility(ctx, r.cfg.WorkspaceID, rt.ID, r.cfg.Visibility); err != nil {
			r.cfg.Log.Warn("share runtime with the workspace; retried later", "workspace_id", r.cfg.WorkspaceID, "runtime_id", rt.ID, "error", err)
		}
	}
}

// boundText flattens whitespace and cuts s to at most n bytes, for text that
// came from outside and goes into logs and the inventory.
func boundText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}
