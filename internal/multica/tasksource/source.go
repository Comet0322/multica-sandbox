// Package tasksource is the manager's task source for Multica: to the
// Multica server it is one daemon, the bot account's own. It follows the
// account's workspaces and turns each into a tenant for the core, registers
// and keeps the runtimes in each, heartbeats them, reads the pending lists,
// claims and fails tasks when the core says so, and answers the pod side's
// questions about registrations directly, since they do not touch the policy.
package tasksource

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/policy"
)

// DefaultSyncInterval is how often the source looks for workspaces its
// account joined or left, the stock daemon's own rhythm.
const DefaultSyncInterval = 30 * time.Second

// DefaultHeartbeatInterval is how often the source heartbeats its runtimes,
// well inside the server's 150-second freshness window.
const DefaultHeartbeatInterval = 15 * time.Second

// syncRemoveAfter is how many listings in a row must leave a workspace out
// before it is removed. Multica reads the list from a replica that may lag,
// so one listing short of a workspace is not taken as the account leaving it.
const syncRemoveAfter = 2

// Config configures a Source.
type Config struct {
	Log *slog.Logger
	Now func() time.Time
	// Client is the bot account on the Multica server.
	Client *multica.Client
	// Providers limits which agent CLIs a pod reports become upstream
	// runtimes. Empty exposes every reported provider.
	Providers []string
	// RuntimeVisibility is set on every runtime registered: "public" or
	// "private".
	RuntimeVisibility string
	// Runtimes are the runtime kinds registered in every workspace, each
	// under its own daemon_id (see DaemonIDSuffix) and machine. The pool
	// kind's machine name, else the first kind's, is a workspace's default
	// runtime name. Empty registers the pool kind alone, under no name.
	Runtimes []RuntimeSet
	// Quota is the default of every workspace.
	Quota int
	// SyncInterval is how often SyncLoop lists the workspaces. Zero means
	// DefaultSyncInterval.
	SyncInterval time.Duration
	// HeartbeatInterval is how often the runtimes are heartbeated. Zero
	// means DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// Models are model list reports, by provider, that answer the picker
	// for every runtime of that provider; no pod is asked for it. A provider
	// not listed is answered from a pod's report (see multica.ModelCatalog).
	Models map[string]json.RawMessage
	// ReadOverrides returns the per-workspace overrides file, read on every
	// Sync. Nil means no overrides.
	ReadOverrides func() ([]byte, error)
}

// RuntimeSet is the runtimes of one kind the source registers in every
// workspace.
type RuntimeSet struct {
	Kind policy.Kind
	// Machine is how these runtimes appear in the runtime list. For the pool
	// kind a workspace's own runtime name, when it has one, comes first.
	multica.Machine
}

// DaemonIDSuffix follows the manager's identity in the daemon_id of a
// kind's runtimes: none for the pool kind, which keeps the identity the
// manager always had, "-<kind>" for any other.
func DaemonIDSuffix(kind policy.Kind) string {
	if kind == policy.KindPool {
		return ""
	}
	return "-" + string(kind)
}

// Core is what the source tells the core.
type Core interface {
	SetTenants(ctx context.Context, list []core.Tenant) core.TenantChanges
}

// Pods is what the source tells the pod side directly: nothing of it touches
// the policy.
type Pods interface {
	// NotifyWorkspacesChanged makes the pods' daemons read their workspace
	// list again: at once, or rate-limited.
	NotifyWorkspacesChanged(now bool)
	// NoteModelRequest hands a model list request Multica opened for a
	// runtime to the pods.
	NoteModelRequest(runtimeID, id string)
	// HasLivePods reports whether any pod is alive to register a workspace.
	HasLivePods() bool
}

// Source is the Multica task source.
type Source struct {
	cfg  Config
	log  *slog.Logger
	up   *multica.Client
	core Core
	pods Pods
	ws   *workspaceSet

	// reloadMu serializes reloads.
	reloadMu sync.Mutex

	mu sync.Mutex
	// overrides are the per-workspace overrides in effect.
	overrides overrideState
	// models is the latest pod model list report per provider not in
	// cfg.Models, reused for every runtime of that provider.
	models *multica.ModelCatalog
}

// New returns the source. Connect gives it the core and the pod side before
// it serves anything.
func New(cfg Config) *Source {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = DefaultSyncInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if len(cfg.Runtimes) == 0 {
		cfg.Runtimes = []RuntimeSet{{Kind: policy.KindPool}}
	}
	return &Source{cfg: cfg, log: cfg.Log, up: cfg.Client, ws: newWorkspaceSet(cfg.Log), models: multica.NewModelCatalog(cfg.Now, 0)}
}

// defaultName is a workspace's runtime name when it sets none: the pool
// kind's machine name, else the first kind's.
func (s *Source) defaultName() string {
	for _, set := range s.cfg.Runtimes {
		if set.Kind == policy.KindPool {
			return set.Name
		}
	}
	return s.cfg.Runtimes[0].Name
}

// Connect gives the source the core and the pod side.
func (s *Source) Connect(c Core, pods Pods) {
	s.core, s.pods = c, pods
}

func (s *Source) now() time.Time { return s.cfg.Now() }

// Client is the bot account's client.
func (s *Source) Client() *multica.Client { return s.up }

// Discovery. The source serves every workspace the bot account belongs to,
// the way a member's daemon does: it lists them, registers a local runtime in
// each, and follows the account as it joins or leaves workspaces. Pods learn
// the same list from the pod side, so a new workspace needs no configuration
// anywhere.

// listWorkspaces asks Multica which workspaces the bot account belongs to.
func (s *Source) listWorkspaces(ctx context.Context) ([]WorkspaceConfig, error) {
	listed, err := s.up.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	// To the server the manager is one machine, whichever workspaces it
	// serves; resolve that identity before any workspace is served.
	if _, err := s.up.Identity(ctx); err != nil {
		return nil, err
	}
	out := make([]WorkspaceConfig, 0, len(listed))
	for _, w := range listed {
		out = append(out, WorkspaceConfig{WorkspaceID: w.ID, Slug: w.Slug, RuntimeName: s.defaultName()})
	}
	return out, nil
}

// SyncStatus is the outcome of the latest workspace listing.
type SyncStatus struct {
	OK    bool      `json:"ok"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// NotReady explains why the source is not ready, or returns "" when it is: a
// listing must have succeeded within three sync intervals. That is what turns
// a revoked or expired PAT, or an unreachable Multica, into a visible signal;
// serving goes on meanwhile.
func (s *Source) NotReady() string {
	return s.ws.notReady(s.now(), s.cfg.SyncInterval)
}

// Sync brings the served workspaces in line with the account's memberships.
// Added ones are claimed from at once; left ones drain once syncRemoveAfter
// listings in a row left them out. A failed listing, or one that comes back
// empty, changes nothing: the account losing every workspace at once is far
// likelier to be an error than a decision.
//
// Each pass also retries what may have failed on the way: the visibility of
// registered runtimes, and the hint that sends pods to register a workspace
// that has no registration yet.
func (s *Source) Sync(ctx context.Context) (ReloadResult, error) {
	list, err := s.listWorkspaces(ctx)
	if err != nil {
		s.log.Warn("list workspaces; keeping the current ones", "error", err)
		s.ws.recordSync(err, s.now())
		return ReloadResult{}, err
	}
	if len(list) == 0 {
		s.log.Warn("the account belongs to no workspace; keeping the current ones")
		s.ws.recordSync(fmt.Errorf("the account belongs to no workspace"), s.now())
		return ReloadResult{}, nil
	}
	list = s.ws.debounce(list)
	s.resolveWorkspaceSettings(ctx, list)
	res, err := s.reload(ctx, list)
	s.ws.recordSync(err, s.now())
	if err == nil && (len(res.Added) > 0 || len(res.Removed) > 0) {
		s.pods.NotifyWorkspacesChanged(true)
	} else if s.unregisteredWithPods() {
		// A pod's registration for the workspace failed (Multica was down,
		// say); the stock daemon would look again only in 30 minutes.
		s.pods.NotifyWorkspacesChanged(false)
	}
	s.reconcileVisibility(ctx)
	return res, err
}

// SyncLoop runs Sync every SyncInterval, and whenever now fires, until ctx
// ends.
func (s *Source) SyncLoop(ctx context.Context, now <-chan struct{}) {
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-now:
		}
		_, _ = s.Sync(ctx)
	}
}

// Reload replaces the served workspaces with list, without the listing's
// debounce or the overrides file. An invalid list is refused whole and the
// current configuration stays in effect.
func (s *Source) Reload(ctx context.Context, list []WorkspaceConfig) (ReloadResult, error) {
	if _, err := s.up.Identity(ctx); err != nil {
		return ReloadResult{}, err
	}
	return s.reload(ctx, list)
}

// reload replaces the served workspaces and tells the core. A removed
// workspace is no longer claimed from; the core drains it and closes it here
// (Close) once its work ended.
func (s *Source) reload(ctx context.Context, workspaces []WorkspaceConfig) (ReloadResult, error) {
	if len(workspaces) == 0 {
		return ReloadResult{}, fmt.Errorf("workspace config lists no workspaces")
	}
	next := slices.Clone(workspaces)
	seen := map[string]bool{}
	for i := range next {
		if err := next[i].normalize(s.defaultName()); err != nil {
			return ReloadResult{}, err
		}
		if seen[next[i].WorkspaceID] {
			return ReloadResult{}, fmt.Errorf("workspace %s is configured twice", next[i].WorkspaceID)
		}
		seen[next[i].WorkspaceID] = true
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	res := s.ws.apply(next, func(wc WorkspaceConfig) *workspace {
		ws := &workspace{cfg: wc}
		for _, set := range s.cfg.Runtimes {
			name := set.Name
			if set.Kind == policy.KindPool && wc.RuntimeName != "" {
				name = wc.RuntimeName
			}
			ws.sets = append(ws.sets, kindSet{kind: set.Kind, rt: s.up.Runtimes(multica.RuntimesConfig{
				WorkspaceID: wc.WorkspaceID, Name: name, DeviceName: set.DeviceName, DaemonIDSuffix: DaemonIDSuffix(set.Kind),
				Providers: s.cfg.Providers, Visibility: s.cfg.RuntimeVisibility, Log: s.log, Now: s.now,
			})})
		}
		return ws
	})
	tenants := make([]core.Tenant, 0, len(next))
	for _, ws := range s.ws.served() {
		tenants = append(tenants, core.Tenant{ID: ws.cfg.WorkspaceID, Settings: s.ws.config(ws).Settings()})
	}
	s.core.SetTenants(ctx, tenants)
	// Sync reloads every pass; only a pass that changed something is news.
	logAt := s.log.Debug
	if len(res.Added)+len(res.Removed)+len(res.Updated)+len(res.Unapplied) > 0 {
		logAt = s.log.Info
	}
	logAt("workspace configuration reloaded",
		"workspaces", len(next), "added", res.Added, "removed", res.Removed,
		"settings_updated", res.Updated, "restart_needed", res.Unapplied)
	return res, nil
}

// Close forgets a removed workspace whose drain ended, and disconnects it.
func (s *Source) Close(tenant string) {
	if ws := s.ws.finishDrain(tenant); ws != nil {
		ws.close()
	}
}

// unregisteredWithPods reports whether some served workspace has no upstream
// registration while live pods are in the pool to register it.
func (s *Source) unregisteredWithPods() bool {
	if !s.pods.HasLivePods() {
		return false
	}
	for _, ws := range s.ws.served() {
		for _, set := range ws.sets {
			if !set.rt.Registered() {
				return true
			}
		}
	}
	return false
}

// reconcileVisibility makes sure every served workspace's registered runtimes
// have the wanted visibility (see multica.Runtimes.EnsureVisibility).
func (s *Source) reconcileVisibility(ctx context.Context) {
	for _, ws := range s.ws.served() {
		for _, set := range ws.sets {
			set.rt.EnsureVisibility(ctx)
		}
	}
}

// What the core asks.

// Claimable lists the served workspaces that have runtimes, in rotation
// order.
func (s *Source) Claimable() []string {
	var out []string
	for _, ws := range s.ws.served() {
		for _, set := range ws.sets {
			if len(set.rt.IDs()) > 0 {
				out = append(out, ws.cfg.WorkspaceID)
				break
			}
		}
	}
	return out
}

// Pending reads a workspace's pending list. The server keeps one per runtime,
// so a workspace costs one read per manager runtime there; the lists are
// merged.
func (s *Source) Pending(ctx context.Context, tenant string, kind policy.Kind) ([]policy.Pending, error) {
	ws := s.ws.get(tenant)
	if ws == nil {
		return nil, fmt.Errorf("workspace %s is not served", tenant)
	}
	rt := ws.of(kind)
	if rt == nil {
		return nil, fmt.Errorf("workspace %s has no %s runtimes", tenant, kind)
	}
	tasks, err := rt.Pending(ctx, rt.IDs())
	if err != nil {
		return nil, err
	}
	list := make([]policy.Pending, 0, len(tasks))
	for _, t := range tasks {
		list = append(list, policy.Pending{ID: t.ID, Priority: t.Priority, CreatedAt: t.Created(), Status: t.Status})
	}
	return list, nil
}

// Claim claims at most one task from a workspace's runtimes. A task the
// server returns without an ID is left for the server to hand out again.
func (s *Source) Claim(ctx context.Context, tenant string, kind policy.Kind) (*core.Task, error) {
	ws := s.ws.get(tenant)
	if ws == nil {
		return nil, fmt.Errorf("workspace %s is not served", tenant)
	}
	rt := ws.of(kind)
	if rt == nil {
		return nil, fmt.Errorf("workspace %s has no %s runtimes", tenant, kind)
	}
	ids := rt.IDs()
	if len(ids) == 0 {
		return nil, nil // not registered yet
	}
	raw, err := rt.Claim(ctx, ids)
	if err != nil || raw == nil {
		return nil, err
	}
	claimed, err := multica.ParseClaimed(raw)
	if err != nil {
		s.log.Error("claimed task has no ID; it will be reclaimed by the server", "workspace_id", tenant)
		return nil, nil
	}
	owner := strings.ToLower(claimed.WorkspaceID)
	return &core.Task{Task: claimed.PolicyTask(owner, kind), Email: claimed.Email(), Payload: claimed.Raw}, nil
}

// Fail reports a claimed task as failed. It returns the error when the
// report did not get through for a reason worth sending it again (the
// network, 429 or 5xx); Multica refusing it otherwise means it no longer
// needs the report, and a workspace no longer served cannot take one.
func (s *Source) Fail(ctx context.Context, tenant, taskID, reason string) error {
	ws := s.ws.find(tenant)
	if ws == nil {
		s.log.Warn("fail task upstream: its workspace is not served; not reported", "task_id", taskID, "workspace_id", tenant)
		return nil
	}
	err := ws.conn().Fail(ctx, taskID, reason)
	switch {
	case err == nil:
		return nil
	case multica.RetryableFail(err):
		s.log.Warn("fail task upstream; sent again later", "task_id", taskID, "error", err)
		return err
	default:
		s.log.Warn("fail task upstream: refused; not sent again", "task_id", taskID, "error", err)
		return nil
	}
}

// TaskEnded reports whether a leased task is over upstream. A task that ended
// (completed, failed, cancelled, or no longer found) is over; so is one whose
// workspace the PAT lost access to, which cannot be reported on any more. Any
// other error leaves it for the next pass.
func (s *Source) TaskEnded(ctx context.Context, tenant, taskID string) (string, bool) {
	ws := s.ws.find(tenant)
	if ws == nil {
		return "", false
	}
	status, err := ws.conn().TaskStatus(ctx, taskID)
	switch code := multica.StatusOf(err); {
	case err == nil && multica.TerminalTaskStatus(status):
		return "upstream task ended: " + status, true
	case code == http.StatusNotFound:
		return "upstream task ended: not found", true
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Sprintf("upstream access lost (HTTP %d)", code), true
	case err != nil:
		s.log.Debug("reconcile lease: task status unavailable; kept", "task_id", taskID, "error", err)
	}
	return "", false
}

// HeartbeatLoop heartbeats the registered runtimes every HeartbeatInterval
// until ctx ends. The source is the one daemon Multica sees, so its runtimes
// are online for as long as it runs; with no pod, nothing is claimed and
// tasks stay queued in Multica.
func (s *Source) HeartbeatLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.HeartbeatOnce(ctx)
		}
	}
}

// HeartbeatOnce heartbeats every registered runtime once. A model list
// request in the answer is answered as answerModels says; a runtime Multica
// no longer knows drops its workspace's registration.
func (s *Source) HeartbeatOnce(ctx context.Context) {
	for _, ws := range append(s.ws.served(), s.ws.drainingList()...) {
		for _, set := range ws.sets {
			for _, id := range set.rt.IDs() {
				modelRequest, err := set.rt.Heartbeat(ctx, id)
				if modelRequest != "" {
					s.answerModels(ctx, set.rt, id, modelRequest)
				}
				if err != nil {
					s.log.Warn("heartbeat runtime upstream", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", id, "error", err)
				}
				if multica.IsRuntimeNotFound(err) {
					s.dropRegistration(ws, set.rt, id)
				}
			}
		}
	}
}

// answerModels answers a model list request. A provider the operator listed
// is answered from that list alone and never by a pod. Any other is answered
// from the catalog's pod report: the pods run one image, so any pod's report
// is every runtime's. Without one, or when Multica does not take it, a pod is
// asked.
func (s *Source) answerModels(ctx context.Context, rt *multica.Runtimes, runtimeID, requestID string) {
	provider := rt.Provider(runtimeID)
	if report, ok := s.cfg.Models[provider]; ok {
		s.postModels(ctx, rt, runtimeID, requestID, report, "configuration")
		return
	}
	if report, ok := s.models.Report(provider); ok && s.postModels(ctx, rt, runtimeID, requestID, report, "catalog") {
		return
	}
	s.pods.NoteModelRequest(runtimeID, requestID)
}

// postModels sends a model list report upstream and reports whether Multica
// took it.
func (s *Source) postModels(ctx context.Context, rt *multica.Runtimes, runtimeID, requestID string, report json.RawMessage, from string) bool {
	err := rt.Conn().Post(ctx, multica.ModelReportPath(runtimeID, requestID), report, nil)
	if err != nil {
		s.log.Warn("answer model list request", "from", from, "runtime_id", runtimeID, "request_id", requestID, "error", err)
		return false
	}
	s.log.Debug("model list request answered", "from", from, "runtime_id", runtimeID, "request_id", requestID)
	return true
}

// ModelsReported keeps a pod's model list report, which Multica took, for
// later requests of every runtime with the same provider.
func (s *Source) ModelsReported(runtimeID string, report []byte) {
	ws := s.ws.byRuntime(runtimeID)
	if ws == nil {
		return
	}
	if provider := ws.owning(runtimeID).Provider(runtimeID); s.models.Remember(provider, report) {
		s.log.Info("model list kept for the provider's runtimes", "provider", provider, "runtime_id", runtimeID)
	}
}

// dropRegistration forgets a workspace's registration after Multica answered
// that one of its runtimes is gone, and hints the pods to register the
// workspace again (see multica.Runtimes.Drop).
func (s *Source) dropRegistration(ws *workspace, rt *multica.Runtimes, runtimeID string) {
	if !rt.Drop(runtimeID) {
		return
	}
	s.log.Warn("runtime gone upstream; the workspace registers again with the next pod", "workspace_id", ws.cfg.WorkspaceID, "runtime_id", runtimeID)
	s.pods.NotifyWorkspacesChanged(false)
}

// What the pod side asks, directly.

// Tenants lists the served workspaces in rotation order, as the daemon lists
// them.
func (s *Source) Tenants() []multica.WorkspaceRef {
	order := s.ws.served()
	out := make([]multica.WorkspaceRef, 0, len(order))
	for _, ws := range order {
		out = append(out, multica.WorkspaceRef{ID: ws.cfg.WorkspaceID, Name: ws.cfg.RuntimeName})
	}
	return out
}

// Served returns the canonical ID of a served workspace.
func (s *Source) Served(id string) (string, bool) {
	ws := s.ws.get(id)
	if ws == nil {
		return "", false
	}
	return ws.cfg.WorkspaceID, true
}

// Register registers the workspace's runtimes upstream on behalf of a pod's
// daemon the first time (and when the pod offers an agent CLI the
// registration lacks), and returns the answer for that pod, filtered to the
// agent CLIs it offers, with the canonical workspace ID. A daemon that names
// no workspace registers the only one, when there is only one.
func (s *Source) Register(ctx context.Context, podID string, req multica.Registration) (map[string]json.RawMessage, string, *multica.PodError) {
	unavailable := &multica.PodError{Status: http.StatusServiceUnavailable, Msg: "runtime registration unavailable"}
	ws := s.ws.get(req.WorkspaceID)
	if order := s.ws.served(); ws == nil && req.WorkspaceID == "" && len(order) == 1 {
		ws = order[0]
	}
	if ws == nil {
		return nil, "", multica.Refuse("workspace is not served by this sandbox manager")
	}
	id := ws.cfg.WorkspaceID
	// The pod's daemon hosts the runtimes of every kind: it runs a task only
	// for a runtime it hosts, and the pod may be given a task of any kind.
	var resp map[string]json.RawMessage
	var runtimes []json.RawMessage
	for _, set := range ws.sets {
		registration, err := set.rt.Register(ctx, req.Raw, req.Headers)
		if err != nil {
			s.log.Warn("register the workspace's runtimes from a pod", "pod_id", podID, "workspace_id", id, "kind", set.kind, "error", err)
			return nil, "", unavailable
		}
		answer, ok := multica.AnswerForPod(registration, req.Offered)
		var these []json.RawMessage
		if !ok || json.Unmarshal(answer["runtimes"], &these) != nil {
			return nil, "", unavailable
		}
		if resp == nil {
			resp = answer
		}
		runtimes = append(runtimes, these...)
	}
	if runtimes == nil {
		runtimes = []json.RawMessage{}
	}
	resp["runtimes"], _ = json.Marshal(runtimes)
	return resp, id, nil
}

// RuntimeServed reports whether a served workspace owns the runtime.
func (s *Source) RuntimeServed(runtimeID string) bool {
	for _, ws := range s.ws.served() {
		if ws.owning(runtimeID) != nil {
			return true
		}
	}
	return false
}

// RuntimeWorkspace is the served workspace one of the manager's runtimes
// belongs to.
func (s *Source) RuntimeWorkspace(runtimeID string) (string, bool) {
	for _, ws := range s.ws.served() {
		if ws.owning(runtimeID) != nil {
			return ws.cfg.WorkspaceID, true
		}
	}
	return "", false
}

// RuntimeGone reports whether Multica said the runtime no longer exists.
func (s *Source) RuntimeGone(runtimeID string) bool {
	for _, ws := range s.ws.served() {
		for _, set := range ws.sets {
			if set.rt.Gone(runtimeID) {
				return true
			}
		}
	}
	return false
}

// What the inventory reads.

// WorkspaceView is one workspace as the source knows it.
type WorkspaceView struct {
	Config   WorkspaceConfig
	Upstream *multica.Health
}

// Workspaces lists the served workspaces in rotation order, and the removed
// ones still draining.
func (s *Source) Workspaces() (served, draining []WorkspaceView) {
	for _, ws := range s.ws.served() {
		served = append(served, WorkspaceView{Config: s.ws.config(ws), Upstream: ws.health()})
	}
	for _, ws := range s.ws.drainingList() {
		draining = append(draining, WorkspaceView{Config: s.ws.config(ws), Upstream: ws.health()})
	}
	return served, draining
}

// LastSync is the outcome of the latest listing; nil before the first.
func (s *Source) LastSync() *SyncStatus { return s.ws.syncStatus() }
