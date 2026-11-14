//go:build linux

package podsupervisor_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeTree creates each file (and its parents) under root.
func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755))
		must(t, os.WriteFile(filepath.Join(root, f), []byte(f), 0o644))
	}
}

func assertExists(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s should have survived the reset: %v", p, err)
		}
	}
}

func TestResetWithTheDefaultSkipListKeepsOnlyTheDaemonConfig(t *testing.T) {
	const config = `{"server_url":"http://manager","token":"pod-token-written-at-start"}`
	p := startPod(t, "", func(p *pod) {
		// What the entrypoint writes at container start, before the supervisor.
		must(t, os.MkdirAll(filepath.Join(p.daemonHome, ".multica"), 0o700))
		must(t, os.WriteFile(filepath.Join(p.daemonHome, ".multica", "config.json"), []byte(config), 0o600))
	})
	p.waitDaemonStarts(1)

	outside := filepath.Join(t.TempDir(), "outside")
	writeTree(t, outside, "keep.txt")
	// What a task leaves: its workspace, temporary files, the agent's caches.
	writeTree(t, p.daemonHome, "file", ".hidden", "a/b/.c/deep", ".cache/x",
		"workspaces/ws/task/repo/f", "tmp/multica-task-1/f", ".npm/_cacache/x")
	must(t, os.Symlink(outside, filepath.Join(p.daemonHome, "link-to-outside")))
	must(t, os.Chmod(filepath.Join(p.daemonHome, "a", "b"), 0o500))

	rep := p.ctl.reset(t, "r1")
	if rep.Status != "done" || len(rep.Failed) != 0 {
		t.Fatalf("reset report = %+v, want done with nothing failed", rep)
	}
	if got := readFile(t, filepath.Join(p.daemonHome, ".multica", "config.json")); got != config {
		t.Errorf("daemon config = %q, want the one written at start", got)
	}
	for _, gone := range []string{"file", ".hidden", "a", ".cache", ".npm", "workspaces", "link-to-outside"} {
		if _, err := os.Lstat(filepath.Join(p.daemonHome, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset: %v", gone, err)
		}
	}
	assertEmptyDir(t, filepath.Join(p.daemonHome, "tmp"))
	assertExists(t, filepath.Join(outside, "keep.txt"))
}

// assertEmptyDir checks that dir exists and holds nothing.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("%s should exist: %v", dir, err)
		return
	}
	if len(entries) != 0 {
		t.Errorf("%s not empty: %v", dir, entries)
	}
}

// The daemon's TMPDIR is under the daemon home, so the task temporary files in
// it go with a reset, and it is there again, empty, for the next task.
func TestResetEmptiesTheDaemonTmpdirAndLeavesItInPlace(t *testing.T) {
	p := startPod(t, "", nil)
	p.waitDaemonStarts(1)
	tmp := filepath.Join(p.daemonHome, "tmp")
	writeTree(t, tmp, "multica-task-1/.task_lock", "multica-task-1/build/out", ".hidden")

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" || len(rep.Failed) != 0 {
		t.Fatalf("reset report = %+v", rep)
	}
	assertEmptyDir(t, tmp)
	if fi, err := os.Stat(tmp); err == nil && fi.Mode().Perm() != 0o700 {
		t.Errorf("TMPDIR mode = %v, want 0700", fi.Mode().Perm())
	}
}

// A container restart keeps the pod's volumes, so the supervisor's start clears
// the daemon home the same way a reset does.
func TestStartClearsTheDaemonHomeLikeAReset(t *testing.T) {
	p := startPod(t, "", func(p *pod) {
		writeTree(t, p.daemonHome, ".multica/config.json", "workspaces/ws/left", "tmp/left", ".cache/left")
	})
	p.waitDaemonStarts(1)
	assertExists(t, filepath.Join(p.daemonHome, ".multica", "config.json"))
	for _, gone := range []string{"workspaces", ".cache"} {
		if _, err := os.Lstat(filepath.Join(p.daemonHome, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the start: %v", gone, err)
		}
	}
	assertEmptyDir(t, filepath.Join(p.daemonHome, "tmp"))
}

func TestResetStopsNoProcess(t *testing.T) {
	mark := "9" + strconv.Itoa(os.Getpid()) + "6"
	pattern := "^sleep " + mark + "\\.[0-9]$"
	p := startPod(t, mark, nil)
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", pattern).Run() })
	p.waitDaemonStarts(1)
	eventually(t, 10*time.Second, "spawned processes", func() bool { return pgrepCount(t, pattern) == 3 })
	daemon := p.daemonPids()[0]

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" {
		t.Fatalf("reset report = %+v", rep)
	}
	time.Sleep(300 * time.Millisecond)
	if !alive(daemon) || len(p.daemonPids()) != 1 {
		t.Errorf("the reset stopped or restarted the daemon: pids %v", p.daemonPids())
	}
	if n := pgrepCount(t, pattern); n != 3 {
		t.Errorf("%d of 3 descendants survived the reset", n)
	}
	for _, e := range p.ctl.events() {
		if e == "daemon-exited" {
			t.Errorf("the daemon exited during the reset: events %v", p.ctl.events())
		}
	}
}

func TestResetHonoursAConfiguredSkipListOnly(t *testing.T) {
	p := startPod(t, "", func(p *pod) { p.args = []string{"-daemon-home-skip", "keep"} })
	p.waitDaemonStarts(1)
	writeTree(t, p.daemonHome, "keep/state", ".multica/config.json", "other")

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" || len(rep.Failed) != 0 {
		t.Fatalf("reset report = %+v", rep)
	}
	assertExists(t, filepath.Join(p.daemonHome, "keep", "state"))
	for _, gone := range []string{".multica", "other"} {
		if _, err := os.Lstat(filepath.Join(p.daemonHome, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is not on the skip list but survived the reset: %v", gone, err)
		}
	}
}

// A skip entry may name a nested path: the reset keeps exactly that path and
// deletes its siblings and the rest of its parent, like the agent's sessions
// and credentials next to the image's skills.
func TestResetKeepsANestedSkipPathAndDeletesTheRestOfItsParent(t *testing.T) {
	p := startPod(t, "", func(p *pod) { p.args = []string{"-daemon-home-skip", ".multica,.claude/skills"} })
	p.waitDaemonStarts(1)
	writeTree(t, p.daemonHome, ".multica/config.json", ".claude/skills/image-skill/SKILL.md",
		".claude/.credentials.json", ".claude/projects/task/session.jsonl", ".claude/skillsx/f",
		".claude.json", "other")

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" || len(rep.Failed) != 0 {
		t.Fatalf("reset report = %+v", rep)
	}
	assertExists(t, filepath.Join(p.daemonHome, ".multica", "config.json"),
		filepath.Join(p.daemonHome, ".claude", "skills", "image-skill", "SKILL.md"))
	entries, err := os.ReadDir(filepath.Join(p.daemonHome, ".claude"))
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "skills" {
		t.Errorf(".claude after the reset holds %v, want only skills", entries)
	}
	for _, gone := range []string{".claude.json", "other"} {
		if _, err := os.Lstat(filepath.Join(p.daemonHome, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is not on the skip list but survived the reset: %v", gone, err)
		}
	}
}

// The entrypoint copies the image's Claude skills in before the supervisor
// starts; the default skip list keeps them through the start and every reset.
func TestTheDefaultSkipListKeepsTheImageSkillsThroughResets(t *testing.T) {
	p := startPod(t, "", func(p *pod) {
		writeTree(t, p.daemonHome, ".multica/config.json", ".claude/skills/image-skill/SKILL.md")
	})
	p.waitDaemonStarts(1)
	skill := filepath.Join(p.daemonHome, ".claude", "skills", "image-skill", "SKILL.md")
	for i, id := range []string{"r1", "r2"} {
		writeTree(t, p.daemonHome, ".claude/projects/task/session.jsonl", ".claude/history.jsonl")
		if rep := p.ctl.reset(t, id); rep.Status != "done" || len(rep.Failed) != 0 {
			t.Fatalf("reset %d report = %+v", i+1, rep)
		}
		assertExists(t, skill, filepath.Join(p.daemonHome, ".multica", "config.json"))
		for _, gone := range []string{".claude/projects", ".claude/history.jsonl"} {
			if _, err := os.Lstat(filepath.Join(p.daemonHome, gone)); !os.IsNotExist(err) {
				t.Errorf("reset %d: %s survived: %v", i+1, gone, err)
			}
		}
	}
}

// A nested skip entry keeps nothing reached through a symlink: a link in place
// of its parent directory is deleted, not followed.
func TestResetDeletesASymlinkInPlaceOfANestedSkipParent(t *testing.T) {
	p := startPod(t, "", func(p *pod) { p.args = []string{"-daemon-home-skip", ".claude/skills"} })
	p.waitDaemonStarts(1)
	outside := filepath.Join(t.TempDir(), "outside")
	writeTree(t, outside, "skills/keep.txt", "other.txt")
	must(t, os.Symlink(outside, filepath.Join(p.daemonHome, ".claude")))

	if rep := p.ctl.reset(t, "r1"); rep.Status != "done" || len(rep.Failed) != 0 {
		t.Fatalf("reset report = %+v", rep)
	}
	if _, err := os.Lstat(filepath.Join(p.daemonHome, ".claude")); !os.IsNotExist(err) {
		t.Errorf("the .claude symlink survived the reset: %v", err)
	}
	assertExists(t, filepath.Join(outside, "skills", "keep.txt"), filepath.Join(outside, "other.txt"))
}

// A mount point cannot be removed, like a volume mounted inside the daemon
// home. The supervisor runs as root of its own user and mount namespace so it
// can mount one without privileges.
func TestResetCarriesOnPastWhatItCannotDeleteAndReportsIt(t *testing.T) {
	if err := exec.Command("unshare", "-rm", "true").Run(); err != nil {
		t.Skipf("unprivileged user namespaces unavailable: %v", err)
	}
	var stuck string
	p := startPod(t, "", func(p *pod) {
		stuck = filepath.Join(p.daemonHome, "mounted")
		must(t, os.Mkdir(stuck, 0o755))
		p.wrap = []string{"unshare", "-rm", "sh", "-c", `mount -t tmpfs none "$0" && exec "$@"`, stuck}
	})
	p.waitDaemonStarts(1)
	writeTree(t, p.daemonHome, "a-first", "z-last", "dir/file")

	rep := p.ctl.reset(t, "r1")
	if rep.Status != "done" {
		t.Fatalf("reset report = %+v, want done", rep)
	}
	if len(rep.Failed) != 1 || rep.Failed[0] != stuck {
		t.Errorf("failed paths = %v, want only %s", rep.Failed, stuck)
	}
	for _, gone := range []string{"a-first", "z-last", "dir"} {
		if _, err := os.Lstat(filepath.Join(p.daemonHome, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset: %v", gone, err)
		}
	}
}
