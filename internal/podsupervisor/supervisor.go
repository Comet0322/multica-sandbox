// Package podsupervisor holds the program of a pooled sandbox pod, which is
// one container and no root.
//
// The Supervisor is the container's PID 1. It runs the stock Multica daemon,
// which starts the agent CLIs itself as the same user, and restarts the daemon
// when it crashes. Every place a task writes to lies under the daemon's home
// (Config.DaemonHome): it is HOME of the daemon and the agents (so their
// caches), the daemon's TMPDIR is its tmp subdirectory, and the pod's settings
// put the workspaces root there too. The image's root filesystem is
// read-only.
//
// A reset command only deletes the files in the daemon's home, except the
// paths named in Config.DaemonHomeSkip, and then makes sure TMPDIR
// exists again, empty. No process is stopped, nothing is restored from a
// template, and nothing outside the daemon's home is touched. The
// supervisor's start clears the daemon's home the same way, since a container
// restart keeps the pod's volumes.
//
// # Control channel
//
// The supervisor polls Config.ControlURL with GET every PollInterval,
// presenting Config.PodToken as its bearer token: the pod token the manager
// issued when the pod's entrypoint registered it. The response is JSON, or an empty body / 204 when there is nothing to do:
//
//	{"id": "r-42", "command": "reset"}
//
// Unknown commands are ignored. For "reset" the supervisor performs the reset,
// carrying on past anything it cannot delete, and then POSTs the outcome to
// the same URL, listing the paths it could not delete:
//
//	{"id": "r-42", "status": "done"}
//	{"id": "r-42", "status": "done", "failed": ["/home/pod/x"]}
//
// The server should keep returning a command until its outcome has been
// reported; a command whose id was already handled is not executed twice, its
// stored outcome is only re-sent.
//
// The supervisor also reports events, with no command id, so the server can
// tell a daemon that restarted from one that kept running:
//
//	{"event": "started"}        the supervisor (so the container) started
//	{"event": "daemon-exited"}  the daemon exited by itself and is restarted
//
// "started" is sent before the daemon is first started. "daemon-exited" is
// sent before the daemon is started again, but a manager that cannot be
// reached does not hold the restart up for long.
//
// A control URL that answers 401 (the manager refuses the pod token: it
// restarted, or it dropped the pod) makes the supervisor exit at once, and one
// that keeps answering 404 does so after Config.UnknownTimeout, so the
// container restarts and its entrypoint registers it as a new pod.
//
// # Isolation
//
// The daemon and the agents run as the supervisor's user. The agents are not
// isolated from the daemon: they can see and trace it, and read its home,
// including the daemon's CLI config. What keeps the supervisor out of their
// reach:
//
//   - it sets PR_SET_DUMPABLE to 0, so processes of the same user can
//     neither ptrace it nor read /proc/<pid>/{mem,environ,fd,...};
//   - as PID 1 it ignores SIGKILL and SIGSTOP from inside the pod (the
//     kernel drops them) and ignores the other signals that would end it
//     except SIGINT and SIGTERM, which only restart the container.
package podsupervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultPollInterval  = 2 * time.Second
	defaultRestartDelay  = time.Second
	zombieReapInterval   = time.Second
	daemonStopTimeout    = 10 * time.Second
	defaultControlClient = 10 * time.Second
	// defaultUnknownTimeout is how long the control URL may answer 404
	// before the supervisor exits. A 401 ends it at once.
	defaultUnknownTimeout = 5 * time.Minute
	// daemonTmpName is the daemon's TMPDIR, a direct child of its home.
	daemonTmpName = "tmp"
)

// DefaultDaemonHomeSkip is the pod's default skip list: the daemon's CLI
// config, with the pod token, and the image's Claude skills, both written at
// container start.
var DefaultDaemonHomeSkip = []string{".multica", ".claude/skills"}

// Config describes the one container of a pod (see the package comment).
type Config struct {
	// DaemonCmd is the command (argv) that starts the daemon.
	DaemonCmd []string
	// Env is the complete base environment of the daemon. The supervisor sets
	// HOME and TMPDIR. Nothing else is inherited, so each restart starts from
	// the same clean environment.
	Env []string

	// DaemonHome is HOME of the daemon and the agents, and the daemon's
	// working directory; the daemon's TMPDIR is its tmp subdirectory. A
	// reset, and the supervisor's start, delete everything in it except the
	// paths named in DaemonHomeSkip, relative to the daemon home (".multica",
	// or a nested path such as ".claude/skills", whose siblings go). Entries
	// that leave the daemon home are refused. Files written there before the
	// supervisor starts (the daemon's CLI config, the image's skills) survive
	// only when the skip list names them.
	DaemonHome     string
	DaemonHomeSkip []string

	ControlURL string
	// PodToken identifies the pod on the control channel.
	PodToken     string
	PollInterval time.Duration
	RestartDelay time.Duration
	// UnknownTimeout is how long the control URL may keep answering 404
	// before the supervisor exits so the container restarts. Zero means
	// defaultUnknownTimeout.
	UnknownTimeout time.Duration
	HTTPClient     *http.Client
	Logger         *slog.Logger
}

// Supervisor runs the daemon and serves reset commands.
type Supervisor struct {
	cfg Config
	log *slog.Logger
	// giveUp makes Run return with the error: the pod cannot be brought back
	// without restarting the container.
	giveUp chan error
}

// New validates cfg.
func New(cfg Config) (*Supervisor, error) {
	var errs []string
	if len(cfg.DaemonCmd) == 0 {
		errs = append(errs, "daemon command is required")
	}
	for name, v := range map[string]string{"control url": cfg.ControlURL, "pod token": cfg.PodToken, "daemon home": cfg.DaemonHome} {
		if v == "" {
			errs = append(errs, name+" is required")
		}
	}
	skip := make([]string, 0, len(cfg.DaemonHomeSkip))
	for _, entry := range cfg.DaemonHomeSkip {
		path := filepath.Clean(entry)
		if entry == "" || path == "." || !filepath.IsLocal(path) {
			errs = append(errs, fmt.Sprintf("daemon home skip entry %q must name a path inside the daemon home", entry))
			continue
		}
		skip = append(skip, filepath.ToSlash(path))
	}
	cfg.DaemonHomeSkip = skip
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.RestartDelay <= 0 {
		cfg.RestartDelay = defaultRestartDelay
	}
	if cfg.UnknownTimeout <= 0 {
		cfg.UnknownTimeout = defaultUnknownTimeout
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultControlClient}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Supervisor{cfg: cfg, log: cfg.Logger, giveUp: make(chan error, 1)}, nil
}

// Run blocks until ctx is cancelled, then stops the daemon and every
// descendant. It returns an error only if the pod cannot be brought up at all.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := becomeSubreaper(); err != nil {
		return fmt.Errorf("become subreaper: %w", err)
	}
	// The agents run as this user and could otherwise read this process's
	// memory and environment or trace it; it must outlive every task.
	if err := denyPeerAccess(); err != nil {
		return fmt.Errorf("make the supervisor undumpable: %w", err)
	}
	setPrivateUmask()
	ignoreStraySignals()
	reserveThreads(32)
	if err := os.MkdirAll(s.cfg.DaemonHome, 0o700); err != nil {
		return fmt.Errorf("prepare the daemon home: %w", err)
	}
	if failed := s.resetDaemonHome(); len(failed) > 0 {
		s.log.Warn("the start could not clear the daemon home", "failed", failed)
	}
	// The server learns of the restart before the daemon calls it.
	s.sendEvent(ctx, eventStarted)
	go s.poll(ctx)

	var (
		cmd     *child
		exited  chan error
		restart <-chan time.Time
	)
	start := func() {
		c, ch, err := s.startDaemon()
		if err != nil {
			s.log.Error("start daemon failed", "error", err)
			restart = time.After(s.cfg.RestartDelay)
			return
		}
		cmd, exited, restart = c, ch, nil
	}
	stop := func() {
		if cmd == nil {
			return
		}
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(daemonStopTimeout):
			s.log.Error("daemon did not exit after SIGKILL")
		}
		cmd, exited = nil, nil
	}

	start()
	reap := time.NewTicker(zombieReapInterval)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			restart = nil
			stop()
			if err := killDescendants(); err != nil {
				s.log.Error("kill descendants on shutdown", "error", err)
			}
			return nil
		case err := <-exited:
			s.log.Warn("daemon exited, restarting", "error", err, "delay", s.cfg.RestartDelay)
			cmd, exited = nil, nil
			// The tasks the daemon ran are gone with it; the server must not
			// take the new daemon for the old one still running them.
			restart = s.afterEvent(ctx, eventDaemonExited, s.cfg.RestartDelay)
		case err := <-s.giveUp:
			restart = nil
			stop()
			if kerr := killDescendants(); kerr != nil {
				s.log.Error("kill descendants before exiting", "error", kerr)
			}
			return err
		case <-restart:
			start()
		case <-reap.C:
			daemon := 0
			if cmd != nil {
				daemon = cmd.Process.Pid
			}
			reapZombies(func(pid int) bool { return pid == daemon })
		}
	}
}

func (s *Supervisor) daemonTmp() string { return filepath.Join(s.cfg.DaemonHome, daemonTmpName) }

// resetDaemonHome deletes everything in the daemon home except the skip
// list, carrying on past what it cannot delete, and returns those paths. The
// daemon's TMPDIR is then there again, empty: the daemon and the agents it
// starts next need it. It is a file operation only: no process is stopped and
// nothing is restored.
func (s *Supervisor) resetDaemonHome() []string {
	var failed []string
	for _, f := range clearDir(s.cfg.DaemonHome, s.cfg.DaemonHomeSkip) {
		s.log.Warn("reset could not delete a path", "path", f.Path, "error", f.Err)
		failed = append(failed, f.Path)
	}
	if err := os.Mkdir(s.daemonTmp(), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		s.log.Warn("reset could not make the daemon's TMPDIR", "path", s.daemonTmp(), "error", err)
		failed = append(failed, s.daemonTmp())
	}
	return failed
}

func (s *Supervisor) startDaemon() (*child, chan error, error) {
	// A process started without a standard input gets none; the daemon reads
	// nothing from it.
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, err
	}
	defer null.Close()
	attr := &os.ProcAttr{
		Dir:   s.cfg.DaemonHome,
		Env:   s.daemonEnv(),
		Files: []*os.File{null, os.Stdout, os.Stderr},
	}
	cmd, err := startChild(&spec{program: s.cfg.DaemonCmd[0], argv0: s.cfg.DaemonCmd[0], args: s.cfg.DaemonCmd[1:], attr: attr})
	if err != nil {
		return nil, nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd, exited, nil
}

func (s *Supervisor) daemonEnv() []string {
	skip := []string{"HOME=", "TMPDIR="}
	env := make([]string, 0, len(s.cfg.Env)+2)
	for _, kv := range s.cfg.Env {
		if !hasAnyPrefix(kv, skip) {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+s.cfg.DaemonHome, "TMPDIR="+s.daemonTmp())
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
