# 06: Supervisor reset deletes daemon home only

**What to build:** A reset is a file operation, nothing more.

- The supervisor deletes everything under the daemon home except the configured skip directories.
- When a delete fails, it logs the error and carries on.
- No file or directory is treated as special beyond the skip list.
- Nothing is restored from a template, and no process is stopped.

The supervisor reports the reset as done, listing the paths it could not delete, and the manager logs them and lends the pod again. The supervisor's configuration is the daemon home and the skip-directory list. The pod's default configuration keeps the files written at container start, including the daemon's configuration that holds the pod token.

**Blocked by:** 01

**Status:** resolved

- [x] After a reset, everything under the daemon home is gone except the skip directories
- [x] A file that cannot be deleted does not stop the reset; the rest is still deleted and the path is reported
- [x] Nothing outside the daemon home is touched, and no template is restored
- [x] No process is stopped by a reset
- [x] The manager logs reported failures and lends the pod again
- [x] The default skip list keeps the daemon's configuration file across resets

## Comments

Verified on branch `worktree-agent-a03e25be9fac47ca9` (2026-11-14), after merging the tip of `feat/sandbox-manager-dispatch` (ticket 02):

- Tests: `go test ./internal/sandboxmanager/ ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/` 267 passed (non-root user); `go vet` and `gofmt` clean on those packages.
  - Supervisor (`reset_daemon_home_test.go`, real binary): the default skip list keeps a `.multica/config.json` written before the supervisor starts and deletes the rest of the daemon home, including read-only trees and a symlink, without following it; work root, shared TMPDIR, agent HOME, `/dev/shm` and the symlink target are untouched; the daemon and its descendants keep running; a configured skip list is the only thing kept; a mount point inside the daemon home (the supervisor run under `unshare -rm`) is reported in `failed` and the rest is still deleted.
  - Manager: the supervisor's `done` report alone returns the pod to idle and it is lent again without the daemon registering; the paths are logged, and audited as "reset completed; N paths could not be deleted".
- Compose smoke (project `t06smoke`, its own Postgres, torn down with `down -v`; scripts in the scratchpad `smoke06/`): two fake pods built from this branch. Both first-seen resets completed on the report alone. A task ran; the pod's planted daemon-home files were deleted, `.multica/config.json` and `tmp` kept, and the daemon PID stayed the same. A second task then ran on the same pod.
- Changed behaviour, as the spec asks: no `failed` report and no retries. The supervisor no longer exits on a failed reset, and the manager no longer waits for a fresh registration (the early-registration handling is gone). The daemon template is copied into the daemon home by the entrypoint at container start (`--daemon-home-template` removed). `POD_DAEMON_HOME_SKIP` overrides the default `.multica,tmp`; `tmp` is skipped because it is the running daemon's TMPDIR.
- **Not done: task files outside the daemon home.** The coordinator asked to point the workspaces root (`MULTICA_WORKSPACES_ROOT=/work/workspaces`) under the daemon home. In the two-container pod merged in ticket 01, that cannot be done without exposing the daemon home: agents run in the runner container, which must see the task directory, and the daemon home (with the pod token) is deliberately not mounted there. As a result, with this change a reset leaves `/work`, the shared `/tmp`, `/dev/shm` and the agent HOME untouched; they are cleaned only at container start. The compose smoke confirmed `/work/workspaces/<ws>/...` task directories survive a reset. This needs a decision: for example, a single-container pod, a work volume mounted at the same path under the daemon home in both containers, or accepting the leftovers.
- Not verified: Kubernetes manifests (the pod's k8s Deployment was not run); a v0.6.4 server.
