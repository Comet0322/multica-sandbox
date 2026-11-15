// Command sandbox-devpool runs the sandbox manager on a simulated pool, for
// development: the core and the task source are the real ones and talk to a
// real Multica server, the pods are simulated (see package devpool). It
// serves only the admin API, with the dispatch page at /dispatch.
//
// It is not for production: nothing runs in a simulated pod, every task is
// reported completed or failed after a random while.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sandbox-manager/internal/core"
	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/multica/devpool"
	"sandbox-manager/internal/multica/tasksource"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-devpool:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		url        = flag.String("multica", "http://127.0.0.1:18204", "Multica server URL")
		tokenFile  = flag.String("token-file", "", "file holding the bot account's PAT (or set SANDBOX_POOL_TOKEN)")
		admin      = flag.String("admin", "127.0.0.1:8791", "admin listen address (serves /dispatch)")
		overrides  = flag.String("overrides", "", "per-workspace overrides file (guarantee, weight, quota, ...)")
		pods       = flag.Int("pods", 6, "simulated pods")
		taskMin    = flag.Duration("task-min", 20*time.Second, "shortest simulated task")
		taskMax    = flag.Duration("task-max", 90*time.Second, "longest simulated task")
		failRate   = flag.Float64("fail-rate", 0.1, "share of tasks reported failed")
		resetTime  = flag.Duration("reset", 5*time.Second, "how long a simulated reset takes")
		capacity   = flag.Int("capacity", 1, "slots per pod")
		placement  = flag.String("placement", "", "affinity rule (default person-issue)")
		hold       = flag.Duration("hold", 2*time.Minute, "hold timeout")
		lifetime   = flag.Duration("lifetime", 30*time.Minute, "binding maximum lifetime")
		syncEvery  = flag.Duration("sync", 10*time.Second, "workspace listing interval")
		dispatchEv = flag.Duration("dispatch", time.Second, "dispatch tick")
		models     = flag.String("models", envOr("SANDBOX_MODELS", "claude=claude-sonnet-4-6,claude-opus-4-7,claude-haiku-4-5-20251001"), "models the agent picker offers, as SANDBOX_MODELS: provider=model,model;provider=... (the first is the default)")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	pat := strings.TrimSpace(os.Getenv("SANDBOX_POOL_TOKEN"))
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("token file: %w", err)
		}
		pat = strings.TrimSpace(string(raw))
	}
	if pat == "" {
		return fmt.Errorf("the bot account's PAT is required: -token-file or SANDBOX_POOL_TOKEN")
	}
	cfg := manager.Config{
		MulticaURL: *url, PAT: pat, Name: os.Getenv("SANDBOX_MANAGER_NAME"),
		DeviceName: os.Getenv("SANDBOX_MANAGER_DEVICE_NAME"), ExpectedSize: *pods,
		Capacity: *capacity, AffinityRule: lending.AffinityRule(*placement),
		HoldTimeout: *hold, MaxLifetime: *lifetime, SyncInterval: *syncEvery,
		DispatchInterval: *dispatchEv, ReconcileInterval: 30 * time.Second, Providers: []string{"claude"},
	}
	var err error
	if cfg.Models, err = multica.ParseModels(*models); err != nil {
		return fmt.Errorf("-models: %w", err)
	}
	if *overrides != "" {
		path := *overrides
		cfg.ReadOverrides = func() ([]byte, error) { return os.ReadFile(path) }
	}
	m, err := manager.NewSimulated(cfg, log, func(c *core.Core, s *tasksource.Source, client *multica.Client) (manager.PodSide, func(context.Context)) {
		p := devpool.New(devpool.Config{
			Log: log, Pods: *pods, TaskMin: *taskMin, TaskMax: *taskMax, FailRate: *failRate, ResetTime: *resetTime,
		}, c, s, client)
		return p, p.Run
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := m.Sync(ctx); err != nil {
		return fmt.Errorf("list the account's workspaces: %w", err)
	}
	ln, err := net.Listen("tcp", *admin)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: m.AdminHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	log.Info("devpool serving", "dispatch", "http://"+ln.Addr().String()+"/dispatch", "pods", *pods)
	go m.SyncLoop(ctx, nil)
	go m.Run(ctx)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// envOr is the environment variable, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
