package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sandbox-manager/internal/dispatcher"
	"sandbox-manager/internal/lending"
)

// poolSettings is the pool manager's command-line environment. Every variable
// is described in deploy/POOL.md.
type poolSettings struct {
	podListen   string
	adminListen string
	// auditFile is where audit records are appended; "-" means standard output
	// and empty disables auditing.
	auditFile string
	config    dispatcher.PoolConfig
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

func loadPool(getenv func(string) string) (poolSettings, error) {
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
			return dispatcher.DefaultReconcileInterval
		}
		return duration("SANDBOX_POOL_RECONCILE_INTERVAL")
	}
	// A binding holds its pod for 15 minutes by default; "0" resets the pod as
	// soon as its last task ends.
	holdTimeout := func() time.Duration {
		if getenv("SANDBOX_HOLD_TIMEOUT") == "" {
			return dispatcher.DefaultHoldTimeout
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

	s := poolSettings{
		podListen:   getenv("SANDBOX_MANAGER_LISTEN"),
		adminListen: getenv("SANDBOX_POOL_ADMIN_LISTEN"),
		auditFile:   getenv("SANDBOX_AUDIT_FILE"),
		config: dispatcher.PoolConfig{
			MulticaURL:        getenv("MULTICA_SERVER_URL"),
			Ticket:            secret("SANDBOX_POOL_TOKEN"),
			Quota:             whole("SANDBOX_POOL_QUOTA"),
			SyncInterval:      duration("SANDBOX_POOL_SYNC_INTERVAL"),
			RuntimeVisibility: getenv("SANDBOX_POOL_RUNTIME_VISIBILITY"),
			Name:              getenv("SANDBOX_MANAGER_NAME"),
			DaemonID:          strings.TrimSpace(getenv("SANDBOX_MANAGER_DAEMON_ID")),
			ExpectedSize:      whole("SANDBOX_POOL_SIZE"),
			Providers:         list("SANDBOX_PROVIDERS"),
			DrainTimeout:      duration("SANDBOX_POOL_DRAIN_TIMEOUT"),
			HeartbeatTimeout:  duration("SANDBOX_POOL_HEARTBEAT_TIMEOUT"),
			ResetTimeout:      duration("SANDBOX_POOL_RESET_TIMEOUT"),
			LostRetention:     duration("SANDBOX_POOL_LOST_RETENTION"),
			Placement:         lending.AffinityRule(getenv("SANDBOX_PLACEMENT")),
			Capacity:          whole("SANDBOX_POD_CAPACITY"),
			PlacementWait:     duration("SANDBOX_PLACEMENT_WAIT"),
			WakeInterval:      wakeInterval(),
			HoldTimeout:       holdTimeout(),
			PodMaxLifetime:    duration("SANDBOX_POD_MAX_LIFETIME"),
			PodDrainGrace:     duration("SANDBOX_POD_DRAIN_GRACE"),
			ReconcileInterval: reconcileInterval(),
			LeaseMax:          duration("SANDBOX_POOL_LEASE_MAX"),
		},
	}
	switch s.config.RuntimeVisibility {
	case "", "public", "private":
	default:
		fail("SANDBOX_POOL_RUNTIME_VISIBILITY must be public or private")
	}
	if s.config.Ticket == "" {
		fail("SANDBOX_POOL_TOKEN or SANDBOX_POOL_TOKEN_FILE is required")
	}
	if err := dispatcher.CheckPlacement(s.config.Placement); err != nil {
		fail("SANDBOX_PLACEMENT: %v", err)
	}
	// The overrides file is read on every sync, so an edited file applies
	// without a restart; its problems are reported, never fatal.
	if path := getenv("SANDBOX_WORKSPACE_OVERRIDES_FILE"); path != "" {
		if !filepath.IsAbs(path) {
			fail("SANDBOX_WORKSPACE_OVERRIDES_FILE must be an absolute path")
		}
		s.config.ReadOverrides = func() ([]byte, error) { return readConfigFile(path) }
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

// runPool starts the pool manager and looks for workspaces again on SIGHUP
// until ctx ends.
func runPool(ctx context.Context, log *slog.Logger, s poolSettings) error {
	if s.auditFile != "" {
		w, closeAudit, err := openAudit(s.auditFile)
		if err != nil {
			return err
		}
		defer closeAudit()
		s.config.AuditLog = w
	}
	// Range and URL checks of the pool itself run here, before anything
	// listens.
	pool, err := dispatcher.NewPool(s.config, log)
	if err != nil {
		return fmt.Errorf("invalid pool configuration: %w", err)
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

	// The first listing must work: a wrong token or server address stops the
	// manager now instead of leaving it serving nothing.
	if _, err := pool.Sync(ctx); err != nil {
		return fmt.Errorf("list the account's workspaces: %w", err)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return servePool(ctx, log, pool, podLn, adminLn, hup)
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

// servePool serves the pod and admin APIs and looks for workspaces joined or
// left every sync interval and each time reload fires. A sync that fails is
// logged and changes nothing.
func servePool(ctx context.Context, log *slog.Logger, pool *dispatcher.Pool, podLn, adminLn net.Listener, reload <-chan os.Signal) error {
	servers := []*http.Server{
		{Handler: pool.PodHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Handler: pool.AdminHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	listeners := []net.Listener{podLn, adminLn}
	go pool.Run(ctx)
	syncNow := make(chan struct{}, 1)
	go pool.SyncLoop(ctx, syncNow)
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
	log.InfoContext(ctx, "sandbox pool manager started", "pod_listen", podLn.Addr().String(), "admin_listen", adminLn.Addr().String())
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("pool API stopped: %w", err)
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
