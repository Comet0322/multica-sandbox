# 15: Image skills survive reset

**What to build:** Ticket 14 made a reset delete all of HOME, so the Claude skills baked into the pod image (`deploy/sandbox/home/.claude/skills`, plus the fake image's `hidden-runtime-skill`) no longer reach agents. User decision (2026-11-14): keep them through the skip list, not by restoring a template.

- The entrypoint copies the image's skills into the daemon home at container start, next to the daemon's `.multica` config.
- Skip entries may name a nested path relative to the daemon home, such as `.claude/skills`, not only a direct child. A reset keeps that path and deletes everything else, including the rest of `.claude` (sessions, history, credentials).
- The default skip list is `.multica` and `.claude/skills`.
- Everything else from ticket 06 is unchanged. Reset stays a file operation that carries on past failures and reports the paths it failed to delete, and nothing is restored on reset.

The agent can read the pod token in `.multica`. That is accepted, as the user decided on 2026-11-14.

**Blocked by:** 14

**Status:** resolved

- [x] A nested skip entry keeps exactly that path; its siblings and the rest of its parent are deleted
- [x] Skip entries that escape the daemon home (`..`, absolute paths) are refused at startup
- [x] At container start the image's skills are present under `$HOME/.claude/skills` and survive every reset
- [x] Compose smoke: the fake agent reports the image skill on a second task after a reset, and finds no earlier task files

## Comments

Verified on branch `worktree-agent-afe6f5c198a9b620e` (2026-11-14):

- Tests (non-root user): `go test -count=1 ./internal/podsupervisor/... ./cmd/pod-supervisor/...` 24 passed; `go vet` and `gofmt` clean; `GOOS=darwin go build` of both works.
  - New (real supervisor binary): a nested skip entry `.claude/skills` keeps exactly that path; `.claude/.credentials.json`, `.claude/projects/...`, a sibling `.claude/skillsx`, `.claude.json` and other files go, and `.claude` then holds only `skills`. The default skip list keeps the image skills written before the supervisor starts through the start and two resets, while `.claude/projects` and `.claude/history.jsonl` go each time. A symlink in place of `.claude` is deleted, not followed; its target is untouched.
  - Config: `.claude/skills` is accepted; `""`, `.`, `..`, `../x`, `.claude/../../x`, `/etc` and `/home/pod/.multica` are refused by `podsupervisor.New` (so the supervisor exits at startup).
- Image: `docker build -f deploy/sandbox/Dockerfile.pool --target fake` builds; `Dockerfile.manager` builds. The default (agents) target was not rebuilt.
- Compose smoke (project `t15smoke`, its own Postgres, backend `multica-backend:pooldev`, torn down with `down -v`; scripts in the scratchpad `smoke15/`): one fake pod. After the first-seen reset `/home/pod` held `.multica`, `tmp` and `.claude/skills/{hidden-runtime-skill,sandbox-environment}`. Four tasks ran on the one pod, each followed by a reset the manager logged as completed; every agent comment listed both skills, and tasks 1 to 3 found 0 earlier task files. After each reset `.claude` held only `skills`. The pod was recreated once with the volume kept, to check the entrypoint replaces existing skills. Control: a file planted in `.claude/projects` of the idle pod (no reset before the next lease) was found by the next task (count 1).

Changes:
- Skip entries are paths relative to the daemon home (cleaned, then must be `filepath.IsLocal` and not `.`). The reset descends into a parent of a nested entry only when it is a real directory; anything else in its place is deleted.
- Default skip list `.multica,.claude/skills` (`DefaultDaemonHomeSkip`).
- `entrypoint.sh`: a separate block after the daemon-template copy replaces `$POD_DAEMON_HOME/.claude/skills` with the image's `/etc/multica-sandbox/home-template/.claude/skills` (which includes the fake image's `hidden-runtime-skill`).
- The fake agent also leaves a file in `~/.claude/projects`, so the smoke shows the rest of `.claude` is deleted.
- Docs: `SANDBOX.md`, `POOL.md`, `home/README.md`, `Dockerfile.pool` comment.

Known risk: an agent can write into `.claude/skills` (it runs as the pod user), and that survives resets until the container restarts, so it can reach later tasks of other people, like anything written into `.multica`. Recorded in `SANDBOX.md` under the skip list.

Not verified: k8s Deployment on a cluster; a v0.6.4 server; the default (real agents) image with real Claude Code reading the skills.
