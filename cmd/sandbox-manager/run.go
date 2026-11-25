package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/policy"
	"sandbox-manager/internal/store"
)

// managerSettings is the manager's command-line environment. Every variable
// is described in docs/DEPLOYMENT.md.
type managerSettings struct {
	podListen   string
	adminListen string
	// auditFile is where audit records are appended; "-" means standard output
	// and empty disables auditing.
	auditFile string
	// databaseURL is the Postgres database the dispatch record is kept in,
	// and payloadKey the AES-256 key task payloads are encrypted with there.
	databaseURL string
	payloadKey  []byte
	config      manager.Config
	// warnings name the settings that are set but no longer used.
	warnings []string
}

// removedSettings are the settings of the GitOps pool, which pods created
// through the Kubernetes API replaced.
// One still set is ignored with a warning, so an old deployment starts.
var removedSettings = []string{
	"SANDBOX_POOL_SIZE",
	"SANDBOX_POD_CAPACITY",
	"SANDBOX_PLACEMENT", "SANDBOX_PLACEMENT_WAIT",
	"SANDBOX_POD_MAX_LIFETIME", "SANDBOX_POD_DRAIN_GRACE",
	"SANDBOX_POOL_RESET_TIMEOUT", "SANDBOX_POOL_HEARTBEAT_TIMEOUT", "SANDBOX_POOL_LOST_RETENTION",
	"SANDBOX_POOL_DRAIN_TIMEOUT", "SANDBOX_POOL_LEASE_MAX",
}

// secretValue reads a secret from NAME or, when NAME_FILE is set, from that
// file. Setting both is refused. Errors name the variable, never the value.
func secretValue(getenv func(string) string, name string) (string, error) {
	value, path := getenv(name), getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	}
	if path == "" {
		return strings.TrimSpace(value), nil
	}
	raw, err := readConfigFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: cannot read %s", name, path)
	}
	return strings.TrimSpace(string(raw)), nil
}

func loadSettings(getenv func(string) string) (managerSettings, error) {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	duration := func(name string) time.Duration {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			fail("%s must be a duration such as 10m", name)
		} else if d < 0 {
			fail("%s must not be negative", name)
		}
		return d
	}
	// The dispatch tick is 1s by default; "0" keeps that tick and only disables
	// the wake socket.
	wakeInterval := func() time.Duration {
		if getenv("SANDBOX_POOL_WAKE_INTERVAL") == "" {
			return time.Second
		}
		return duration("SANDBOX_POOL_WAKE_INTERVAL")
	}
	// Lease reconciliation is on by default too; "0" turns it off.
	reconcileInterval := func() time.Duration {
		if getenv("SANDBOX_POOL_RECONCILE_INTERVAL") == "" {
			return manager.DefaultReconcileInterval
		}
		return duration("SANDBOX_POOL_RECONCILE_INTERVAL")
	}
	// A pod is held for its task's key for 10 minutes by default; "0" holds
	// nothing.
	holdTimeout := func() time.Duration {
		if getenv("SANDBOX_HOLD_TIMEOUT") == "" {
			return manager.DefaultHoldTimeout
		}
		return duration("SANDBOX_HOLD_TIMEOUT")
	}
	whole := func(name string) int {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fail("%s must be a whole number, 0 or more", name)
			return 0
		}
		return n
	}
	list := func(name string) []string {
		var out []string
		for _, item := range strings.Split(getenv(name), ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	secret := func(name string) string {
		v, err := secretValue(getenv, name)
		if err != nil {
			fail("%s", err)
		}
		return v
	}

	s := managerSettings{
		podListen:   getenv("SANDBOX_MANAGER_LISTEN"),
		adminListen: getenv("SANDBOX_POOL_ADMIN_LISTEN"),
		auditFile:   getenv("SANDBOX_AUDIT_FILE"),
		config: manager.Config{
			MulticaURL:        getenv("MULTICA_SERVER_URL"),
			PAT:               secret("SANDBOX_POOL_TOKEN"),
			Quota:             whole("SANDBOX_POOL_QUOTA"),
			SyncInterval:      duration("SANDBOX_POOL_SYNC_INTERVAL"),
			RuntimeVisibility: getenv("SANDBOX_POOL_RUNTIME_VISIBILITY"),
			Pool:              multica.Machine{Name: getenv("SANDBOX_MANAGER_NAME"), DeviceName: getenv("SANDBOX_MANAGER_DEVICE_NAME")},
			Clean:             multica.Machine{Name: getenv("SANDBOX_MANAGER_CLEAN_NAME"), DeviceName: getenv("SANDBOX_MANAGER_CLEAN_DEVICE_NAME")},
			DaemonID:          strings.TrimSpace(getenv("SANDBOX_MANAGER_DAEMON_ID")),
			Providers:         list("SANDBOX_PROVIDERS"),
			DispatchInterval:  wakeInterval(),
			HoldTimeout:       holdTimeout(),
			PodMaxIdle:        duration("SANDBOX_POD_MAX_IDLE"),
			ReconcileInterval: reconcileInterval(),
		},
	}
	for _, name := range removedSettings {
		if getenv(name) != "" {
			s.warnings = append(s.warnings, multica.NoLongerUsed(name))
		}
	}
	switch s.config.RuntimeVisibility {
	case "", "public", "private":
	default:
		fail("SANDBOX_POOL_RUNTIME_VISIBILITY must be public or private")
	}
	if s.config.PAT == "" {
		fail("SANDBOX_POOL_TOKEN or SANDBOX_POOL_TOKEN_FILE is required")
	}
	// The dispatch record: a restarted manager takes over from it.
	if s.databaseURL = secret("SANDBOX_DATABASE_URL"); s.databaseURL == "" {
		fail("SANDBOX_DATABASE_URL or SANDBOX_DATABASE_URL_FILE is required: the Postgres database the manager keeps its dispatch record in")
	}
	if path := getenv("SANDBOX_PAYLOAD_KEY_FILE"); path == "" {
		fail("SANDBOX_PAYLOAD_KEY_FILE is required: the key task payloads are encrypted with in the database")
	} else if key, err := payloadKey(path); err != nil {
		fail("SANDBOX_PAYLOAD_KEY_FILE: %v", err)
	} else {
		s.payloadKey = key
	}
	models, err := multica.ParseModels(getenv("SANDBOX_MODELS"))
	if err != nil {
		fail("SANDBOX_MODELS: %v", err)
	}
	s.config.Models = models
	kinds, err := runtimeKinds(getenv("SANDBOX_RUNTIMES"))
	if err != nil {
		fail("SANDBOX_RUNTIMES: %v", err)
	}
	s.config.Runtimes = kinds
	// The overrides file is read on every sync, so an edited file applies
	// without a restart; its problems are reported, never fatal.
	if path := getenv("SANDBOX_WORKSPACE_OVERRIDES_FILE"); path != "" {
		if !filepath.IsAbs(path) {
			fail("SANDBOX_WORKSPACE_OVERRIDES_FILE must be an absolute path")
		}
		s.config.ReadOverrides = func() ([]byte, error) { return readConfigFile(path) }
	}

	// Pods are created through the Kubernetes API; two warm pods
	// unless set. At least one: a workspace's runtimes are learnt from a
	// pod's daemon registering them, so with no pod at all nothing would
	// ever be claimed.
	warm := 2
	if getenv("SANDBOX_WARM_PODS") != "" {
		warm = whole("SANDBOX_WARM_PODS")
		if warm == 0 {
			fail("SANDBOX_WARM_PODS must be at least 1: the manager learns each workspace's runtimes from a pod's daemon")
		}
	}
	if getenv("SANDBOX_POD_TEMPLATE") == "" {
		fail("SANDBOX_POD_TEMPLATE is required: the pod YAML the manager creates its pods from")
	} else if kc, err := kubeSettings(getenv, whole("SANDBOX_MAX_PODS"), warm, duration("SANDBOX_POD_START_TIMEOUT")); err != nil {
		fail("%v", err)
	} else {
		// Unset or 0: sandbox.DefaultStartFailures.
		kc.StartFailures = whole("SANDBOX_POD_START_FAILURES")
		owner, err := podOwner(getenv("SANDBOX_POD_OWNER"))
		if err != nil {
			fail("SANDBOX_POD_OWNER: %v", err)
		}
		kc.OwnerDeployment = owner
		s.config.Kube = kc
	}

	if s.podListen == "" {
		s.podListen = ":8790"
	}
	if s.adminListen == "" {
		// The admin interface is not for pods; stay on loopback by default.
		s.adminListen = "127.0.0.1:8791"
	}
	if len(errs) > 0 {
		return s, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return s, nil
}

// runManager starts the manager and looks for workspaces again on SIGHUP
// until ctx ends.
func runManager(ctx context.Context, log *slog.Logger, s managerSettings) error {
	for _, w := range s.warnings {
		log.Warn(w)
	}
	if s.auditFile != "" {
		w, closeAudit, err := openAudit(s.auditFile)
		if err != nil {
			return err
		}
		defer closeAudit()
		s.config.AuditLog = w
	}
	db, err := openStore(ctx, store.Config{URL: s.databaseURL, PayloadKey: s.payloadKey, Log: log})
	if err != nil {
		return fmt.Errorf("open the dispatch record's database (SANDBOX_DATABASE_URL): %w", err)
	}
	defer db.Close()
	go db.PruneLoop(ctx)
	s.config.Store = db
	// Range and URL checks of the manager itself run here, before anything
	// listens.
	m, err := manager.New(s.config, log)
	if err != nil {
		return fmt.Errorf("invalid manager configuration: %w", err)
	}
	podLn, err := net.Listen("tcp", s.podListen)
	if err != nil {
		return fmt.Errorf("listen for pods: %w", err)
	}
	defer podLn.Close()
	adminLn, err := net.Listen("tcp", s.adminListen)
	if err != nil {
		return fmt.Errorf("listen for administrators: %w", err)
	}
	defer adminLn.Close()

	// What a previous run left is taken over from the dispatch record before
	// anything is claimed; the pods' owner is read first.
	if err := m.Restore(ctx); err != nil {
		return fmt.Errorf("take over from the previous run: %w", err)
	}
	// The first listing must work: a wrong token or server address stops the
	// manager now instead of leaving it serving nothing.
	if _, err := m.Sync(ctx); err != nil {
		return fmt.Errorf("list the account's workspaces: %w", err)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return serveManager(ctx, log, m, podLn, adminLn, hup)
}

// recordStore is the dispatch record's database as the command line uses
// it.
type recordStore interface {
	core.Store
	PruneLoop(ctx context.Context)
	Close()
}

// openStore opens the dispatch record's database; tests replace it.
var openStore = func(ctx context.Context, cfg store.Config) (recordStore, error) {
	return store.Open(ctx, cfg)
}

// payloadKey reads SANDBOX_PAYLOAD_KEY_FILE: 32 bytes in standard base64,
// as "openssl rand -base64 32" writes them. Its errors never show the key.
func payloadKey(path string) ([]byte, error) {
	raw, err := readConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s does not hold base64 (write it with: openssl rand -base64 32)", path)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s holds a %d-byte key, want 32 (write it with: openssl rand -base64 32)", path, len(key))
	}
	return key, nil
}

// openAudit opens the audit log for appending. "-" is standard output. The
// file is private to the manager's user: it names people and tasks.
func openAudit(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := openAppendFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("SANDBOX_AUDIT_FILE: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// serveManager serves the pod and admin APIs and looks for workspaces joined or
// left every sync interval and each time reload fires. A sync that fails is
// logged and changes nothing.
func serveManager(ctx context.Context, log *slog.Logger, m *manager.Manager, podLn, adminLn net.Listener, reload <-chan os.Signal) error {
	servers := []*http.Server{
		{Handler: m.PodHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Handler: m.AdminHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	listeners := []net.Listener{podLn, adminLn}
	go m.Run(ctx)
	syncNow := make(chan struct{}, 1)
	go m.SyncLoop(ctx, syncNow)
	serveErr := make(chan error, len(servers))
	for i, ln := range listeners {
		go func() { serveErr <- servers[i].Serve(ln) }()
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
	}()
	log.InfoContext(ctx, "sandbox manager started", "pod_listen", podLn.Addr().String(), "admin_listen", adminLn.Addr().String())
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("manager API stopped: %w", err)
		case <-reload:
			log.InfoContext(ctx, "looking for workspaces and reading the overrides file on request")
			select {
			case syncNow <- struct{}{}:
			default:
			}
		}
	}
}

// configFile splits a file setting into the directory that holds it and the
// file's name, opened as a root: every access then stays inside that
// directory. Settings name mounted files (Secrets, volumes), so a relative path
// is a mistake, not a convenience.
func configFile(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", fmt.Errorf("%q is not an absolute path", path)
	}
	dir, name := filepath.Split(filepath.Clean(path))
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	return root, name, nil
}

// readConfigFile reads a file setting.
func readConfigFile(path string) ([]byte, error) {
	root, name, err := configFile(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}

// openAppendFile opens a file setting for appending, creating it private to the
// user.
func openAppendFile(path string) (*os.File, error) {
	root, name, err := configFile(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// runtimeKinds reads SANDBOX_RUNTIMES: pool, clean or both, comma
// separated. Empty means pool.
func runtimeKinds(v string) ([]policy.Kind, error) {
	var out []policy.Kind
	for _, item := range strings.Split(v, ",") {
		k := policy.Kind(strings.ToLower(strings.TrimSpace(item)))
		switch {
		case k == "":
			continue
		case k != policy.KindPool && k != policy.KindClean:
			return nil, fmt.Errorf("unknown runtime kind %q (want pool, clean or pool,clean)", item)
		case slices.Contains(out, k):
			return nil, fmt.Errorf("%s is listed twice", k)
		}
		out = append(out, k)
	}
	return policy.KindsOrPool(out), nil
}
