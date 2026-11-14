package podsupervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const killDeadline = 10 * time.Second

// killDescendants SIGKILLs every descendant of this process until none is
// left, reaping them as it goes. It loops because a dying process can fork
// once more before the signal lands.
func killDescendants() error {
	self := os.Getpid()
	deadline := time.Now().Add(killDeadline)
	for {
		procs := descendants(self)
		if len(procs) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d descendant processes survived SIGKILL", len(procs))
		}
		for _, p := range procs {
			if !p.zombie {
				killProc(p.pid)
			}
		}
		for _, p := range procs {
			reapProc(p.pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reapZombies collects exited orphans that were reparented to the supervisor,
// leaving alone the processes keep names (the daemon and the agents the
// supervisor started, each reaped by its own Wait).
func reapZombies(keep func(pid int) bool) {
	self := os.Getpid()
	for _, p := range listProcs() {
		if p.ppid == self && p.zombie && !keep(p.pid) {
			reapProc(p.pid)
		}
	}
}

// clearDir removes everything inside dir except the paths named in keep, and
// keeps dir itself. A keep entry is a slash-separated path relative to dir,
// such as ".claude/skills": that path is kept and everything else in its
// parents is removed. A parent that is not a real directory (a symlink) is
// removed like any other entry, never followed. clearDir carries on past
// anything it cannot remove and returns those paths with the cause. Hidden
// files, symlinks (never followed) and read-only nested directories are all
// removed. Every step goes through an os.Root, so a link planted while the
// tree is being emptied cannot lead the removal outside dir.
func clearDir(dir string, keep []string) []*fs.PathError {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return []*fs.PathError{pathError(dir, err)}
	}
	defer root.Close()
	return clearRoot(root, dir, keep)
}

func clearRoot(root *os.Root, base string, keep []string) []*fs.PathError {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return []*fs.PathError{pathError(base, err)}
	}
	var failed []*fs.PathError
	for _, e := range entries {
		name, path := e.Name(), filepath.Join(base, e.Name())
		if slices.Contains(keep, name) {
			continue
		}
		if below := keptBelow(keep, name); len(below) > 0 {
			if fi, err := root.Lstat(name); err == nil && fi.IsDir() {
				failed = append(failed, clearIn(root, name, path, below)...)
				continue
			}
		}
		failed = append(failed, removeIn(root, name, path)...)
	}
	return failed
}

// keptBelow returns the keep entries under name, relative to name.
func keptBelow(keep []string, name string) []string {
	var below []string
	for _, k := range keep {
		if rest, ok := strings.CutPrefix(k, name+"/"); ok && rest != "" {
			below = append(below, rest)
		}
	}
	return below
}

// clearIn empties the directory name, the direct child of root found at
// path, except the paths in keep, and keeps the directory itself.
func clearIn(root *os.Root, name, path string, keep []string) []*fs.PathError {
	// A read-only directory can neither be listed nor emptied. Only its owner
	// can open it up; for another user's directory the removal below reports
	// the real failure.
	_ = root.Chmod(name, 0o700)
	sub, err := root.OpenRoot(name)
	if err != nil {
		return []*fs.PathError{pathError(path, err)}
	}
	defer sub.Close()
	return clearRoot(sub, path, keep)
}

// removeIn removes name, the direct child of root found at path, with
// everything below it that can be removed.
func removeIn(root *os.Root, name, path string) []*fs.PathError {
	fi, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return []*fs.PathError{pathError(path, err)}
	}
	if fi.IsDir() {
		if failed := clearIn(root, name, path, nil); len(failed) > 0 {
			// The directory cannot go while something is left in it.
			return failed
		}
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return []*fs.PathError{pathError(path, err)}
	}
	return nil
}

// pathError names path, not the os.Root-relative name, with the cause.
func pathError(path string, err error) *fs.PathError {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return &fs.PathError{Op: "remove", Path: path, Err: err}
}
