//go:build linux

package podsupervisor_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"sandbox-manager/internal/podsupervisor"
)

// taskScript is a daemon that, the first time it runs, does what a task's
// agent could do: it leaves files in every per-task location the pod gives it
// (the workspaces root and the caches under HOME, TMPDIR), including a
// read-only tree.
const taskScript = `#!/bin/sh
echo $$ >> "$OUT/pids"
# Written aside and renamed, so a test that sees the file sees all of it.
pwd > "$OUT/.cwd.$$" && mv "$OUT/.cwd.$$" "$OUT/cwd.$$"
env | sort > "$OUT/.env.$$" && mv "$OUT/.env.$$" "$OUT/env.$$"
if [ ! -e "$OUT/spawned" ]; then
  : > "$OUT/spawned"
  mkdir -p "$MULTICA_WORKSPACES_ROOT/ws-a/task-1/repo" "$HOME/.cache/pip" "$TMPDIR/multica-task-1"
  echo task-secret > "$MULTICA_WORKSPACES_ROOT/ws-a/task-1/repo/secret"
  echo task-secret > "$HOME/.cache/pip/secret"
  echo task-secret > "$HOME/.bash_history"
  echo task-secret > "$TMPDIR/multica-task-1/secret"
  echo task-secret > "$TMPDIR/.hidden"
  mkdir -p "$HOME/ro/deep" && echo x > "$HOME/ro/deep/f" && chmod 500 "$HOME/ro/deep" "$HOME/ro"
  : > "$OUT/left"
fi
exec sleep 100000
`

// startTaskPod starts a pod whose daemon is taskScript, with the workspaces
// root under the daemon home as the pod image sets it.
func startTaskPod(t *testing.T) *pod {
	t.Helper()
	return startPod(t, "", func(p *pod) {
		must(t, os.WriteFile(filepath.Join(p.out, "daemon.sh"), []byte(taskScript), 0o755))
		p.env = []string{"MULTICA_WORKSPACES_ROOT=" + filepath.Join(p.daemonHome, "workspaces")}
		writeTree(t, p.daemonHome, ".multica/config.json")
	})
}

func (p *pod) daemonFile(prefix string, pid int) string {
	p.t.Helper()
	var s string
	eventually(p.t, 5*time.Second, prefix+" of the daemon", func() bool {
		raw, err := os.ReadFile(filepath.Join(p.out, prefix+"."+strconv.Itoa(pid)))
		s = string(raw)
		return err == nil
	})
	return s
}

// listTree returns every path under root, relative to it.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	must(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(root, path); rel != "." {
			out = append(out, rel)
		}
		return nil
	}))
	return out
}

// A reset leaves nothing a task created anywhere it can write in a pod, except
// in the skip directories: the next task, of whoever, starts clean.
func TestSingleResetLeavesNoTaskFileBehind(t *testing.T) {
	p := startTaskPod(t)
	p.waitDaemonStarts(1)
	eventually(t, 5*time.Second, "the task's files", func() bool {
		_, err := os.Stat(filepath.Join(p.out, "left"))
		return err == nil
	})

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" || len(rep.Failed) != 0 {
		t.Fatalf("reset report = %+v", rep)
	}
	got := strings.Join(listTree(t, p.daemonHome), " ")
	if want := ".multica .multica/config.json tmp"; got != want {
		t.Errorf("daemon home after reset = %q, want %q", got, want)
	}
}

// Everything a task gets to write to lies under the daemon home: HOME (so
// the agents' caches), TMPDIR, the workspaces root of the pod's settings, and
// the daemon's working directory.
func TestSingleTaskLocationsAreUnderTheDaemonHome(t *testing.T) {
	p := startTaskPod(t)
	p.waitDaemonStarts(1)
	pid := p.daemonPids()[0]
	env := p.daemonFile("env", pid)
	for _, want := range []string{"HOME=" + p.daemonHome, "TMPDIR=" + filepath.Join(p.daemonHome, "tmp"),
		"MULTICA_WORKSPACES_ROOT=" + filepath.Join(p.daemonHome, "workspaces")} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("daemon environment lacks %s:\n%s", want, env)
		}
	}
	if cwd := strings.TrimSpace(p.daemonFile("cwd", pid)); cwd != p.daemonHome {
		t.Errorf("daemon working directory = %s, want the daemon home %s", cwd, p.daemonHome)
	}
}

// A hostile task runs as the supervisor's own user, so the supervisor must not
// be readable or traceable by it, while the processes it starts stay ordinary.
func TestSingleSupervisorIsNotOpenToItsOwnUser(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	sup := p.cmd.Process.Pid
	eventually(t, 5*time.Second, "the supervisor to be undumpable", func() bool {
		_, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", sup))
		return errors.Is(err, os.ErrPermission)
	})
	for _, f := range []string{"mem", "environ", "maps", "fd/0", "cwd", "root"} {
		path := fmt.Sprintf("/proc/%d/%s", sup, f)
		fd, err := os.Open(path)
		if err == nil {
			_ = fd.Close()
			t.Errorf("%s opened by the supervisor's own user", path)
		}
	}
	var perr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		perr = syscall.PtraceAttach(sup)
		if perr == nil {
			_, _ = syscall.Wait4(sup, nil, 0, nil)
			_ = syscall.PtraceDetach(sup)
		}
	}()
	<-done
	if perr == nil {
		t.Errorf("ptrace attach to the supervisor succeeded")
	}
	// The daemon it started is an ordinary, dumpable process (the flag does
	// not survive execve).
	if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", p.daemonPids()[0])); err != nil {
		t.Errorf("daemon environment unreadable: %v", err)
	}
}

// A task that signals everything it can reach, the supervisor included, must
// not stop the next task from getting a clean pod.
func TestSingleSupervisorStillResetsAfterHostileSignals(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	sup := p.cmd.Process.Pid
	// SIGSTOP/SIGKILL from inside a pod are dropped by the kernel for PID 1;
	// here the supervisor is not PID 1, so only the signals it may ignore are
	// sent. SIGUSR1/SIGHUP must not end it.
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGPIPE} {
		_ = syscall.Kill(sup, sig)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(sup, 0); err != nil {
		t.Fatalf("supervisor ended after a stray signal: %v", err)
	}
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
}

func TestConfigNeedsOnlyTheDaemonSettings(t *testing.T) {
	cfg := podsupervisor.Config{DaemonCmd: []string{"/bin/true"}, ControlURL: "http://x", PodToken: "mpt_x", DaemonHome: "/home/pod"}
	if _, err := podsupervisor.New(cfg); err != nil {
		t.Errorf("config rejected: %v", err)
	}
	cfg.PodToken = ""
	if _, err := podsupervisor.New(cfg); err == nil {
		t.Errorf("a config without a pod token is accepted")
	}
	cfg.PodToken = "mpt_x"
	cfg.DaemonHome = ""
	if _, err := podsupervisor.New(cfg); err == nil {
		t.Errorf("a config without a HOME is accepted")
	}
	cfg.DaemonHome, cfg.DaemonHomeSkip = "/home/pod", []string{".multica", ".claude/skills"}
	if _, err := podsupervisor.New(cfg); err != nil {
		t.Errorf("a nested skip entry is rejected: %v", err)
	}
	for _, entry := range []string{"", ".", "..", "../x", ".claude/../../x", "/etc", "/home/pod/.multica"} {
		cfg.DaemonHomeSkip = []string{entry}
		if _, err := podsupervisor.New(cfg); err == nil {
			t.Errorf("skip entry %q, which is not inside the daemon home, is accepted", entry)
		}
	}
}
