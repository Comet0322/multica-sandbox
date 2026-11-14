// pod-supervisor is the PID 1 of a pooled sandbox pod, which is one container
// and no root. It runs the Multica daemon and resets the pod on command. See
// package podsupervisor.
//
// Usage:
//
//	pod-supervisor [flags] -- daemon-command [args...]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sandbox-manager/internal/podsupervisor"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	run, err := parse(os.Args[1:], os.Environ(), log)
	if err != nil {
		log.Error("invalid pod supervisor configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.ErrorContext(ctx, "pod supervisor stopped", "error", err)
		os.Exit(1)
	}
}

// parse reads the flags and returns the function that runs the supervisor.
func parse(args, environ []string, log *slog.Logger) (func(context.Context) error, error) {
	var (
		sup     podsupervisor.Config
		pollGap time.Duration
	)
	fs := flag.NewFlagSet("pod-supervisor", flag.ContinueOnError)
	fs.StringVar(&sup.ControlURL, "control-url", "", "URL polled for reset commands")
	fs.DurationVar(&pollGap, "poll-interval", 2*time.Second, "control URL poll interval")
	fs.StringVar(&sup.DaemonHome, "daemon-home", "", "HOME of the daemon and the agents; a reset clears it")
	daemonConfig := fs.String("daemon-config", "",
		"the daemon's CLI config written at container start, whose token is the pod token (default <daemon-home>/.multica/config.json)")
	skip := fs.String("daemon-home-skip", strings.Join(podsupervisor.DefaultDaemonHomeSkip, ","),
		"comma-separated paths, relative to the daemon HOME, a reset keeps (such as .claude/skills)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	for _, name := range strings.Split(*skip, ",") {
		if name = strings.TrimSpace(name); name != "" {
			sup.DaemonHomeSkip = append(sup.DaemonHomeSkip, name)
		}
	}
	if fs.NArg() == 0 {
		return nil, fmt.Errorf("daemon command required after --")
	}
	if *daemonConfig == "" && sup.DaemonHome != "" {
		*daemonConfig = filepath.Join(sup.DaemonHome, ".multica", "config.json")
	}
	if *daemonConfig != "" {
		token, err := podToken(*daemonConfig)
		if err != nil {
			return nil, err
		}
		sup.PodToken = token
	}
	sup.PollInterval = pollGap
	sup.DaemonCmd = fs.Args()
	sup.Env, sup.Logger = environ, log
	s, err := podsupervisor.New(sup)
	if err != nil {
		return nil, err
	}
	return s.Run, nil
}

// podToken reads the pod token from the daemon's CLI config, where the pod's
// entrypoint wrote it after registering the pod with the manager.
func podToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the pod token: %w", err)
	}
	var cfg struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("read the pod token from %s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return "", fmt.Errorf("%s holds no pod token", path)
	}
	return strings.TrimSpace(cfg.Token), nil
}
