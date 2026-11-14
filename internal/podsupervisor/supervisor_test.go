//go:build linux

package podsupervisor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sandbox-manager/internal/podsupervisor"
)

var supervisorBin string

func TestMain(m *testing.M) {
	if os.Geteuid() == 0 {
		fmt.Println("skipping: tests must run as a non-root user")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "podsupervisor-bin")
	if err != nil {
		panic(err)
	}
	supervisorBin = filepath.Join(dir, "pod-supervisor")
	out, err := exec.Command("go", "build", "-o", supervisorBin, "sandbox-manager/cmd/pod-supervisor").CombinedOutput()
	if err != nil {
		fmt.Printf("build pod-supervisor: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// controlServer plays the manager: it hands out one command at a time and
// records the outcomes the supervisor reports.
type controlServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	pending *podsupervisor.Command
	reports []podsupervisor.Report
	// auth is every Authorization header the supervisor sent, by method.
	auth map[string][]string

	// getStatus, when set, is what every poll answers.
	getStatus int
	// eventStatus, when set, answers the next eventRejects event posts (the
	// events are still recorded).
	eventStatus  int
	eventRejects int
	// onPost sees every post as it arrives.
	onPost func(rep podsupervisor.Report)
}

func newControlServer(t *testing.T) *controlServer {
	t.Helper()
	c := &controlServer{auth: map[string][]string{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.auth[r.Method] = append(c.auth[r.Method], r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodGet:
			if c.getStatus != 0 {
				w.WriteHeader(c.getStatus)
				return
			}
			if c.pending == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(c.pending)
		case http.MethodPost:
			var rep podsupervisor.Report
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &rep); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c.reports = append(c.reports, rep)
			if c.onPost != nil {
				c.onPost(rep)
			}
			if rep.Event != "" && c.eventRejects > 0 {
				c.eventRejects--
				w.WriteHeader(c.eventStatus)
				return
			}
			if c.pending != nil && c.pending.ID == rep.ID {
				c.pending = nil
			}
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// reset issues a reset command and waits for its reported outcome.
func (c *controlServer) reset(t *testing.T, id string) podsupervisor.Report {
	t.Helper()
	c.mu.Lock()
	c.pending = &podsupervisor.Command{ID: id, Command: "reset"}
	c.mu.Unlock()
	var out podsupervisor.Report
	eventually(t, 20*time.Second, "report for "+id, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, r := range c.reports {
			if r.ID == id {
				out = r
				return true
			}
		}
		return false
	})
	return out
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testPodToken is the pod token the manager issued to the test pod.
const testPodToken = "mpt_test_pod_token"

// pod is one pod: the supervisor as a process of the test user, with the flags
// the container gets. The daemon and the agents it starts share one HOME.
type pod struct {
	t          *testing.T
	ctl        *controlServer
	out        string   // test-owned directory, never touched by resets
	daemonHome string   // HOME of the daemon and the agents
	args       []string // extra supervisor flags, set by the setup function
	wrap       []string // command the supervisor runs under, set by the setup function
	env        []string // extra environment
	cmd        *exec.Cmd
}

const daemonScript = `#!/bin/sh
id -u >> "$OUT/uids"
env | sort > "$OUT/env.$$"
echo $$ >> "$OUT/pids"
if [ -n "$SPAWN" ] && [ ! -e "$OUT/spawned" ]; then
  : > "$OUT/spawned"
  export AGENT_SET=leaked
  setsid sleep "$MARK.1" &
  sh -c 'sleep "$0.2" & wait' "$MARK" &
  ( sleep "$MARK.3" & )
fi
exec sleep 100000
`

// startPod launches the real supervisor binary around the stub daemon.
func startPod(t *testing.T, mark string, setup func(p *pod)) *pod {
	t.Helper()
	root := t.TempDir()
	p := &pod{
		t:          t,
		ctl:        newControlServer(t),
		out:        filepath.Join(root, "out"),
		daemonHome: filepath.Join(root, "daemon-home"),
	}
	for _, d := range []string{p.out, p.daemonHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(p.out, "daemon.sh")
	if err := os.WriteFile(script, []byte(daemonScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(p)
	}
	env := append([]string{"PATH=" + os.Getenv("PATH"), "OUT=" + p.out, "MARK=" + mark, "SPAWN=" + map[bool]string{true: "1"}[mark != ""]}, p.env...)

	// The daemon's CLI config the entrypoint writes, with the pod token; it
	// lives outside the daemon home here so no reset in a test removes it.
	config := filepath.Join(p.out, "daemon-config.json")
	if err := os.WriteFile(config, []byte(`{"server_url":"http://manager","token":"`+testPodToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{
		"-control-url", p.ctl.srv.URL,
		"-daemon-config", config,
		"-daemon-home", p.daemonHome,
		"-poll-interval", "50ms",
	}, p.args...)
	argv := append(append(append([]string{}, p.wrap...), supervisorBin), append(args, "--", script)...)
	p.cmd = exec.Command(argv[0], argv[1:]...)
	p.cmd.Env = env
	p.cmd.Stderr = io.Discard
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _ = p.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = p.cmd.Process.Kill()
				<-done
			}
		}
		// Make read-only trees removable for t.TempDir cleanup.
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	return p
}

func (p *pod) daemonPids() []int {
	raw, _ := os.ReadFile(filepath.Join(p.out, "pids"))
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		pids = append(pids, n)
	}
	return pids
}

func (p *pod) waitDaemonStarts(n int) {
	p.t.Helper()
	eventually(p.t, 10*time.Second, fmt.Sprintf("%d daemon starts", n), func() bool { return len(p.daemonPids()) >= n })
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func pgrepCount(t *testing.T, pattern string) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return 0
		}
		t.Fatalf("pgrep: %v", err)
	}
	return len(strings.Fields(string(out)))
}

func TestDaemonRunsAsNonRootAndRestartsOnCrash(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)

	uids, _ := os.ReadFile(filepath.Join(p.out, "uids"))
	if strings.TrimSpace(strings.Fields(string(uids))[0]) == "0" {
		t.Fatalf("daemon ran as root")
	}

	first := p.daemonPids()[0]
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	p.waitDaemonStarts(2)
	if second := p.daemonPids()[1]; second == first || !alive(second) {
		t.Fatalf("daemon was not restarted: pids %v", p.daemonPids())
	}
}

func TestRestartedDaemonGetsFreshEnvironment(t *testing.T) {
	mark := "9" + strconv.Itoa(os.Getpid()) + "2"
	pattern := "^sleep " + mark + "\\.[0-9]$"
	p := startPod(t, mark, nil)
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", pattern).Run() })
	p.waitDaemonStarts(1)
	// The stub exports AGENT_SET only for the processes it spawns.
	eventually(t, 10*time.Second, "agent processes", func() bool { return pgrepCount(t, pattern) == 3 })

	must(t, syscall.Kill(p.daemonPids()[0], syscall.SIGKILL))
	p.waitDaemonStarts(2)
	pids := p.daemonPids()
	first := readFile(t, filepath.Join(p.out, fmt.Sprintf("env.%d", pids[0])))
	second := readFile(t, filepath.Join(p.out, fmt.Sprintf("env.%d", pids[1])))
	if first != second {
		t.Errorf("environment differs across restart:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Contains(second, "AGENT_SET") {
		t.Errorf("agent-set variable survived the restart:\n%s", second)
	}
	for _, want := range []string{"HOME=" + p.daemonHome, "TMPDIR=" + filepath.Join(p.daemonHome, "tmp")} {
		if !strings.Contains(second, want+"\n") {
			t.Errorf("environment lacks %s:\n%s", want, second)
		}
	}
}

func TestReportedCommandIsNotExecutedTwice(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	later := filepath.Join(p.daemonHome, "written-after-the-reset")
	must(t, os.WriteFile(later, []byte("x"), 0o644))

	// A server that keeps offering the same id after the report must not
	// cause another reset.
	p.ctl.mu.Lock()
	p.ctl.pending = &podsupervisor.Command{ID: "r1", Command: "reset"}
	p.ctl.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(later); err != nil {
		t.Errorf("duplicate command re-ran the reset: %v", err)
	}
	p.ctl.mu.Lock()
	var buf bytes.Buffer
	for _, r := range p.ctl.reports {
		fmt.Fprintf(&buf, "%s:%s ", r.ID, r.Status)
	}
	p.ctl.mu.Unlock()
	if !strings.Contains(buf.String(), "r1:done") {
		t.Errorf("reports = %s", buf.String())
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// set changes the server's behaviour while it runs.
func (c *controlServer) set(f func(c *controlServer)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

// events returns the supervisor events received so far, in order.
func (c *controlServer) events() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.reports {
		if r.Event != "" {
			out = append(out, r.Event)
		}
	}
	return out
}

func TestStartedEventComesBeforeTheDaemonStarts(t *testing.T) {
	var daemonRanFirst bool
	p := startPod(t, "", func(p *pod) {
		p.ctl.set(func(c *controlServer) {
			c.onPost = func(rep podsupervisor.Report) { // runs under c.mu
				if rep.Event == "started" {
					if _, err := os.Stat(filepath.Join(p.out, "pids")); err == nil {
						daemonRanFirst = true
					}
				}
			}
		})
	})
	p.waitDaemonStarts(1)
	ev := p.ctl.events()
	if len(ev) == 0 || ev[0] != "started" {
		t.Fatalf("the first event must be started, got %v", ev)
	}
	p.ctl.mu.Lock()
	defer p.ctl.mu.Unlock()
	if daemonRanFirst {
		t.Fatalf("the daemon was already running when the started event arrived")
	}
	if first := p.ctl.reports[0]; first.Event != "started" || first.ID != "" || first.Status != "" {
		t.Fatalf("the started event carries no command id: %+v", first)
	}
}

func TestEventIsRetriedOnServerErrorsThenTheDaemonStartsAnyway(t *testing.T) {
	p := startPod(t, "", func(p *pod) {
		p.ctl.set(func(c *controlServer) { c.eventStatus, c.eventRejects = http.StatusInternalServerError, 100 })
	})
	p.waitDaemonStarts(1)
	started := 0
	for _, e := range p.ctl.events() {
		if e == "started" {
			started++
		}
	}
	if started != 3 {
		t.Fatalf("a 5xx event is sent 3 times and then given up, got %d", started)
	}
}

func TestEventIsNotRetriedOnClientErrors(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusBadRequest} {
		p := startPod(t, "", func(p *pod) {
			p.ctl.set(func(c *controlServer) { c.eventStatus, c.eventRejects = code, 100 })
		})
		p.waitDaemonStarts(1)
		time.Sleep(300 * time.Millisecond)
		if ev := p.ctl.events(); len(ev) != 1 {
			t.Fatalf("a %d answer means delivered (an older manager), got events %v", code, ev)
		}
	}
}

func TestDaemonExitSendsDaemonExited(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)

	must(t, syscall.Kill(p.daemonPids()[0], syscall.SIGKILL))
	eventually(t, 10*time.Second, "daemon-exited event", func() bool {
		for _, e := range p.ctl.events() {
			if e == "daemon-exited" {
				return true
			}
		}
		return false
	})
	p.waitDaemonStarts(2)
}

// A manager that keeps saying it does not know the pod ends the
// supervisor after UnknownTimeout; any other answer restarts the clock. It
// runs the supervisor in-process because the binary has no flag for the
// timeout.
func TestUnknownPodTimeoutEndsTheSupervisor(t *testing.T) {
	run := func(t *testing.T, serve func(n int) int) (time.Duration, error) {
		t.Helper()
		var mu sync.Mutex
		polls := 0
		start := time.Now()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				return
			}
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			w.WriteHeader(serve(n))
		}))
		defer srv.Close()
		root := t.TempDir()
		must(t, os.Mkdir(filepath.Join(root, "daemon-home"), 0o755))
		script := filepath.Join(root, "daemon.sh")
		must(t, os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 100000\n"), 0o755))
		s, err := podsupervisor.New(podsupervisor.Config{
			DaemonCmd: []string{script}, Env: []string{"PATH=" + os.Getenv("PATH")},
			DaemonHome: filepath.Join(root, "daemon-home"),
			ControlURL: srv.URL, PodToken: testPodToken, PollInterval: 20 * time.Millisecond, UnknownTimeout: 300 * time.Millisecond,
		})
		must(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = s.Run(ctx)
		return time.Since(start), err
	}

	t.Run("404 for the whole timeout", func(t *testing.T) {
		took, err := run(t, func(int) int { return http.StatusNotFound })
		if err == nil || took > 4*time.Second {
			t.Fatalf("want an error soon after the timeout, got %v after %s", err, took)
		}
	})
	// The manager refuses a pod token it does not know (it restarted, or it
	// pruned the pod): only a container restart registers the pod again, so
	// waiting would only keep the pod out of the pool.
	t.Run("401 ends it at once", func(t *testing.T) {
		took, err := run(t, func(int) int { return http.StatusUnauthorized })
		if err == nil || took > 200*time.Millisecond {
			t.Fatalf("want an error at the first 401, well before the timeout, got %v after %s", err, took)
		}
	})
	t.Run("one other answer restarts the clock", func(t *testing.T) {
		// 404s for ~200ms, a single 204, then 404s again: without the reset
		// the supervisor would end at ~300ms.
		took, err := run(t, func(n int) int {
			if n == 10 {
				return http.StatusNoContent
			}
			return http.StatusNotFound
		})
		if err == nil || took < 450*time.Millisecond {
			t.Fatalf("the 204 must restart the clock; ended after %s with %v", took, err)
		}
	})
}

// The manager tells pods apart only by their pod token, so the supervisor
// presents the one from the daemon's config on every poll and report.
func TestControlChannelPresentsThePodToken(t *testing.T) {
	p := startPod(t, "", nil)
	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	p.ctl.mu.Lock()
	defer p.ctl.mu.Unlock()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		got := p.ctl.auth[method]
		if len(got) == 0 {
			t.Fatalf("no %s reached the control URL", method)
		}
		for _, h := range got {
			if h != "Bearer "+testPodToken {
				t.Fatalf("%s carried Authorization %q, want the pod token", method, h)
			}
		}
	}
}
