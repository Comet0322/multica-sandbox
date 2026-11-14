//go:build linux

package sandbox_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"sandbox-manager/internal/sandbox"
)

// supervisedPod is the real pod supervisor binary, run as the test user around
// a stub daemon, talking to the service's handlers over HTTP.
type supervisedPod struct {
	*simPod
	t          *testing.T
	out        string
	daemonHome string
}

func buildSupervisor(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pod-supervisor")
	out, err := exec.Command("go", "build", "-o", bin, "sandbox-manager/cmd/pod-supervisor").CombinedOutput()
	if err != nil {
		t.Fatalf("build pod-supervisor: %v\n%s", err, out)
	}
	return bin
}

// startSupervisedPod registers a pod the way its entrypoint does, writes the
// daemon's CLI config with the pod token and starts the supervisor on it.
func startSupervisedPod(t *testing.T, h *harness, bin string) *supervisedPod {
	t.Helper()
	root := t.TempDir()
	p := &supervisedPod{simPod: h.register("pool-0"), t: t, out: filepath.Join(root, "out"), daemonHome: filepath.Join(root, "home")}
	must(t, os.MkdirAll(p.out, 0o755))
	must(t, os.MkdirAll(p.daemonHome, 0o755))
	script := filepath.Join(p.out, "daemon.sh")
	must(t, os.WriteFile(script, []byte("#!/bin/sh\necho $$ >> \"$OUT/pids\"\nexec sleep 100000\n"), 0o755))
	config := filepath.Join(p.out, "config.json")
	must(t, os.WriteFile(config, []byte(`{"server_url":"http://manager","token":"`+p.token+`"}`), 0o600))

	cmd := exec.Command(bin,
		"-control-url", h.srv.URL+sandbox.ControlPath,
		"-daemon-config", config,
		"-daemon-home", p.daemonHome,
		"-poll-interval", "50ms",
		"--", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "OUT=" + p.out}
	must(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return p
}

func (p *supervisedPod) daemonPids() []int {
	raw, _ := os.ReadFile(filepath.Join(p.out, "pids"))
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		pids = append(pids, n)
	}
	return pids
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) eventuallyCreate(want sandbox.PodID) {
	h.t.Helper()
	eventually(h.t, "create to return "+string(want), func() bool {
		id, err := h.create()
		if err != nil && !errors.Is(err, sandbox.ErrNoCapacity) {
			h.t.Fatalf("create: %v", err)
		}
		if err == nil && id != want {
			h.t.Fatalf("create returned %q, want %q", id, want)
		}
		return err == nil
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The supervisor of today, unchanged, runs against the service's handlers:
// it completes the first reset, the reset of a handed-back pod, and reports
// its daemon's restart, which interrupts the pod's holder.
func TestTheRealSupervisorRunsAgainstTheService(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the pod supervisor must run as a non-root user")
	}
	h := newHarness(t)
	p := startSupervisedPod(t, h, buildSupervisor(t))

	// Registered pods are reset first; the supervisor's done report frees it.
	h.eventuallyCreate(p.id)
	if got := h.rec.takeNotices(); len(got) != 0 {
		t.Fatalf("the supervisor's start interrupted a pod nobody held: %+v", got)
	}

	// A task leaves a file behind; handing the pod back clears it.
	eventually(t, "the daemon to start", func() bool { return len(p.daemonPids()) >= 1 })
	leftover := filepath.Join(p.daemonHome, "task-secret")
	must(t, os.WriteFile(leftover, []byte("secret"), 0o600))
	h.delete(p.id)
	h.eventuallyCreate(p.id)
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reset left the task's file behind: %v", err)
	}

	// The daemon dies on its own while the pod is held.
	first := p.daemonPids()[0]
	must(t, syscall.Kill(first, syscall.SIGKILL))
	eventually(t, "the interruption", func() bool {
		h.rec.mu.Lock()
		defer h.rec.mu.Unlock()
		return len(h.rec.notices) > 0
	})
	want := []notice{{"interrupted", p.id, "pod daemon restarted"}}
	if got := h.rec.takeNotices(); !slices.Equal(got, want) {
		t.Fatalf("notices = %+v, want %+v", got, want)
	}
	// The interrupted pod is reset and becomes free again.
	h.eventuallyCreate(p.id)
	eventually(t, "the daemon's restart", func() bool { return len(p.daemonPids()) >= 2 })
}
