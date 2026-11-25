# 14: Single-container pod

**What to build:** A reset must leave nothing of a previous task behind, so that tasks of different people never see each other's files (user story 30). In the two-container pod merged in ticket 01, task directories (`/work/workspaces`), the shared `/tmp` and the agent HOME live outside the daemon home, so a reset (ticket 06) cannot reach them. Replace it with the single-container, non-root pod from the tip of `integration/sandbox-pool`: pod image, supervisor, launcher and shim, entrypoint and pod manifests (k8s and compose).

- Every per-task location (workspaces root, TMPDIR, agent HOME and caches) lives under the daemon home, so a reset clears it.
- Keep the reset semantics from ticket 06. It deletes only under the daemon home, except the skip list (default keeps `.multica`). It carries on past failures, stops no process, and reports "done" with the paths it could not delete.
- Keep everything ticket 02 removed out (no gateway credential helper, no LiteLLM).
- The pod token (ticket 04) lives in `.multica/config.json`. If the integration pod separates the agent user from the daemon user, keep that separation, so the agent cannot read the token. Otherwise, record this as a known risk.

User decision (2026-11-14): this supersedes the earlier choice of the two-container pod from `feat/sandbox-v0.7.0-clean`.

**Blocked by:** 06

**Status:** resolved

- [x] The pod runs as one non-root container in compose and in the k8s manifests; the runner container and its two-container wiring are gone
- [x] Workspaces root, TMPDIR and agent HOME are under the daemon home
- [x] After a reset, no file a previous task created remains, apart from the skip directories
- [x] Ticket 06's reset behaviour and tests still hold
- [x] The supervisor tests from the tip of `integration/sandbox-pool` are carried over, adapted, and pass
- [x] Compose smoke: a task runs, the pod resets, and a second task on the same pod finds none of the first task's files

## Comments

Verified on branch `worktree-agent-af18a65fd245187dd` (2026-11-14):

- Tests (non-root user): `go test -count=1 ./internal/sandboxmanager/... ./cmd/sandbox-manager/... ./internal/podsupervisor/... ./cmd/pod-supervisor/...` 257 passed; `go vet` and `gofmt` clean on the pod packages; `GOOS=darwin go build` of them works.
  - Carried over from `integration/sandbox-pool` and adapted: `supervisor_test.go` (restart on crash, fresh environment, command not run twice, events, unknown-pod timeout), `single_test.go` (undumpable supervisor, stray signals, config), `ipc_linux_test.go`. Dropped: the integration tests that assert the old reset (kills processes, restarts the daemon, restores the HOME template, retries and reports `failed`, empties work root/TMPDIR/shm), and every runner/shim test.
  - New: a task-like daemon leaves files in the workspaces root, HOME caches, TMPDIR and a read-only tree; after a reset the daemon home holds only `.multica/config.json` and an empty `tmp`. Daemon `HOME`, `TMPDIR` and working directory are under the daemon home. The supervisor's start clears the home like a reset. Ticket 06's tests still pass (default skip list, skip list honoured, no process stopped, mount point reported in `failed`).
- Images: `docker build -f deploy/sandbox/Dockerfile.pool --build-arg AGENT_NPM_REGISTRY=https://registry.npmjs.org/ .` and `--target fake` both build; in the default image `/tmp` is a symlink to `/home/pod/tmp`, `/dev/shm` (read-only tmpfs) refuses writes, claude 2.1.323 and opencode 1.19.5 run, and `opencode debug config` shows the provider from `OPENCODE_CONFIG`. `Dockerfile.manager` builds.
- `kubectl kustomize deploy/sandbox/pool/k8s` and `deploy/sandbox/pool/k8s/sandbox` render; the Deployment has one container (uid 1001, read-only root, no capability) with the `home` emptyDir and a read-only `/dev/shm`.
- Compose smoke (project `t14smoke`, its own Postgres, backend `multica-backend:pooldev`, torn down with `down -v`; scripts in the scratchpad `smoke14/`): `stack.yaml` + `pool.yaml` with `--scale pod=1`. The pod registered and its first-seen reset completed. Four fake-agent tasks ran on the one pod, each followed by a reset; the daemon PID stayed the same. The fake agent leaves a file in the task directory, HOME caches, TMPDIR and `/tmp`, and counts any an earlier task left: tasks 2 and 4 found 0. Control: two files planted in the idle pod (no reset before the next lease) were found by task 3 (count 2). Task directories were `/home/pod/workspaces/<ws>/<task>/workdir`; after the last reset `/home/pod` held only `.multica` and an empty `tmp`. `/dev/shm` was read-only for the agent.

Decisions and changes:
- Every per-task location is under `/home/pod`: HOME (agents' caches), the daemon's working directory, `MULTICA_WORKSPACES_ROOT=/home/pod/workspaces`, TMPDIR `/home/pod/tmp` (also `MULTICA_AGENT_TEMP_BASE`, the base of each task's own TMPDIR), and `/tmp` is a symlink there in the image. The root filesystem is read-only and `/dev/shm` is mounted read-only (k8s and compose), so no task file can sit outside HOME.
- Default skip list is now `.multica` only (ticket 06 had `.multica,tmp`). After deleting, the reset makes the empty `tmp` again (mode 0700), because the daemon and the next task need their TMPDIR. This is the one directory the supervisor knows besides the skip list; it is the TMPDIR it sets itself.
- Compose pods come from `pool/compose/pool.yaml` (`--scale pod=N`); `gen-pods.sh` is gone. The k8s Deployment stays at `pool/k8s/sandbox/pool.yaml`.
- `--clean-ipc` clears SysV/POSIX IPC objects at the supervisor's start only, not on a reset.
- The fake agent now reports leftover files and whether shared memory is writable (`deploy/sandbox/fake/fake-claude`).

Known risks / not done:
- **The agent can read the pod token.** The integration pod runs the daemon and the agents as one user (uid 1001); it has no separation to keep. Any agent can read `.multica/config.json` (the token ticket 04 writes there), and see, signal and trace the daemon. The supervisor itself is undumpable.
- **Runtime-private HOME configuration does not reach agents.** A reset deletes HOME and restores no template, and the first-seen reset runs before any task, so `deploy/sandbox/home/` is no longer copied into HOME. opencode still gets its provider config through `OPENCODE_CONFIG`; the Claude Code skills (`.claude/skills/...`, and the fake image's `hidden-runtime-skill`) are not seen (the smoke's comment lists no skill). Needs a decision, for example a skip-listed read-only directory, a CLI setting that points outside HOME, or restoring that template on reset.
- **What a reset still does not reach:** processes a task left running (a reset stops none, per ticket 06; such a process can write files again after the reset), SysV/POSIX IPC objects and `/dev/mqueue` (until the container restarts), and anything a task writes into `.multica`. A read-only `/dev/shm` breaks programs that need POSIX shared memory or semaphores there (for example Python `multiprocessing` locks, Chrome without `--disable-dev-shm-usage`).
- Not verified: the k8s Deployment on a real cluster (only rendered); a v0.6.4 server (the smoke used `multica-backend:pooldev`).

