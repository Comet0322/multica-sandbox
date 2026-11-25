Status: done (read only, 2026-11-26)

# Multica fork delta: security/v0.11.0 against v0.11.0

Goal: the company runs Multica from `security/v0.11.0` (738d38669, 19 commits on the v0.11.0 tag). `multica-v0.11-compat-report.md` compared the manager against the bare tag. This report checks whether those 19 commits change anything the manager depends on.

**Verdict, from the source only.** Nothing the manager sends, relays or answers changes, and nothing breaks. No JSON tag, header name, route, status code, SQL text or env var name changed. The pod's stock daemon sends the same requests. `deploy/Dockerfile.pool` builds the fork's daemon unchanged. The dev namespace already sets the two settings the fork now requires. Every conclusion in the compat report and in issue 01 holds at the fork. The only line citations that move are in `m:server/internal/daemon/daemon.go` (lines after 11 move up by 1) and `m:server/cmd/server/main.go` (lines after 381 move down by 1). Nothing was built or run.

One premise in the brief is wrong. The manager does not dial Multica's daemon WebSocket. Its calls to Multica are plain HTTP (`sm:internal/multica/conn.go`, `client.go`). Outside tests, the only WebSocket code is the server side in `sm:internal/multica/wake.go`, which the pods dial.

## Method

`m:` is `../multica` and `sm:` is this repo. `a → b` is the line at `v0.11.0`, then at `security/v0.11.0`. I read only through git objects (`git show`, `git diff`, `git grep` on the two refs). I checked out nothing and edited nothing in either repo.

Two scripts in the session scratchpad went over every non-test Go file under `m:server/`:

- **Literal diff.** For each file, it compares the string literals on removed lines with those on added lines. Struct tags, header names, route paths, SQL, env var names and map keys are all literals. The only differences are the built-in database URL and JWT messages (rows 2 and 3), URL building in four places (`handler/github.go`, `handler/skill.go`, `cmd/maintenance/main.go`, `cmd/multica/cmd_repo.go`), `"Origin"` in `daemonws/hub.go`, and import paths.
- **Rename residual.** It folds the credscan vocabulary (ticket, sealed, apilock, badge, phrase, carrier) back to token, secret, apikey, credential, password and bearer. It ignores whitespace and cancels removed lines against added ones. Every line left over is a real code change, and each one is a row below or a group in the "irrelevant" list.

What the scripts cannot see, I checked by hand:

- **Untagged struct fields**, whose JSON key is the Go field name. Every renamed untagged field outside sqlc is internal: `daemonws.ClientIdentity`, `handler.Config`, `revocationResult`, `service.InstallCredentials`, `agent.ExecOptions`, the remotemcp OAuth structs and the integration credential structs. None of them is JSON-encoded. sqlc emits JSON tags (`m:server/sqlc.yaml` `emit_json_tags: true`), and they kept their values.
- **SQL inside multi-line raw strings.** Only Go constant names changed in `pkg/db/generated`. The `-- name:` lines and the query text are the same, and no `.sql` file or migration changed.
- **chi 5.3.2 → 5.4.0.** I diffed the module-cache sources. `mux.go` and `tree.go` change routing only by a new panic when a router is mounted onto itself.

## Relevant changes

| # | What changed (commit) | `m:` file:line, v0.11.0 → fork | Manager breaks? | Fix |
| --- | --- | --- | --- | --- |
| 1 | **The daemon WebSocket refuses requests that carry an `Origin` header** (21ad1d87d). | `server/internal/daemonws/hub.go:352 → :352` | No. The manager never opens Multica's daemon WebSocket, so this check never sees it. The pods dial the manager's socket, whose `CheckOrigin` accepts everything (`sm:internal/multica/wake.go:36-39`). The stock daemon sets no `Origin` either: its headers are Authorization and `X-Client-*` (`server/internal/daemon/wakeup.go:105-117` at both refs). | None needed. Optional: copy the check into `sm:wake.go:38` (`r.Header.Get("Origin") == ""`). Pods carry no Origin, so it changes nothing for them. The fork changed this pattern to satisfy a scanner, and the same rule would likely flag the manager in a company SAT run. |
| 2 | **The server and migrator refuse to start without `DATABASE_URL`** (c167c46727). The built-in `multica:multica@localhost` URL is gone. | `server/cmd/server/main.go:378-381 → :378-383`; `server/cmd/migrate/main.go:854-856 → :854-857` | No. The dev namespace puts `DATABASE_URL` in the `multica-backend` Secret (`sm:deploy/k8s/dev/up.sh:43-53`), and the backend reads it through `envFrom` (`sm:deploy/k8s/multica/backend.yaml:39-45`). | None. |
| 3 | **No built-in JWT secret** (c167c46727). Outside production, an empty `JWT_SECRET` now signs with a random key for each process. Production still refuses to boot. | `server/internal/auth/jwt.go:14, 23-26 → :16, 25-36`; boot warning `main.go:343 → :343` | No. The dev namespace sets `JWT_SECRET` (`sm:up.sh:48`). The manager never uses a JWT. It authenticates with the bot account's `mul_` PAT, which the server checks by hash in the database (`server/internal/middleware/daemon_auth.go:269 → :269`). Even with no secret set, the manager would not notice; only the dev setup's browser sessions would end when the backend restarts. | None. |
| 4 | **The backend image runs as uid/gid 10001** (1a6c3af37). | `Dockerfile:44 → :44-49` | No. This matters only if the dev namespace runs a backend built from the fork. Its manifest already forces `runAsUser: 10001`, `fsGroup: 10001`, a read-only root filesystem and emptyDir volumes for `/app/data` and `/tmp` (`sm:deploy/k8s/multica/backend.yaml:30-35, 55-64`). | None. |
| 5 | **Integer clamps on fields the manager sends or relays** (b2c357f9e). `max_tasks` and message `seq` now saturate at int32 instead of wrapping, and so does `since_seq` on the message lists. | `server/internal/service/task.go:4346 → :4346`; `server/internal/handler/daemon.go:6012 → :6012` | No. The manager sends `max_tasks: 1` (`sm:conn.go:186`). `seq` is the pod daemon's own counter, which the manager relays unchanged. The two differ only above 2^31. | None. |
| 6 | **Credscan renames** (9eba02eaf, plus a203c3973 and 484d382d0): 490 files rename Go identifiers. | Wire fields the manager or the pod touches: `handler/agent.go:425, 639` (`remote_mcp_daemon_token`, `auth_token`); `daemon/types.go:92, 205`; `cli/config.go:27` (`token`); `daemon/client.go:243-244` (`credential_header`, `credential`); `middleware/daemon_auth.go:29` (`"daemon_token"`), `:160` (`mdt_`), `:269` (`mul_`). Each is at the same line at both refs. | No. Every JSON tag and wire string above is unchanged (see the literal diff). The pod's `.multica/config.json` keeps the `token` key that `sm:deploy/Dockerfile.pool:89` writes. Task-token paths and the `mat_` prefix are unchanged, so `sm:internal/multica/task.go:100-106` (`TaskTokenPath`) and `daemonapi.go:201` (`isAgentPath`) still hold. | None. |
| 7 | **Pod daemon hardening** (b2c357f9e, f032c0d37). File operations go through `util.Stat/ReadFile/WriteFile/MkdirAll`, which refuse only empty or NUL paths, clean the rest lexically and make the same OS call. Jitter now draws from crypto/rand with the same range. The daemon's health server gets `ReadHeaderTimeout: 10s`. The agent CLI's `repo checkout` builds the same `http://127.0.0.1:<port>/repo/checkout` URL another way. | `server/internal/util/fs.go`, `num.go` (new); `daemon/health.go:459 → :459`; `daemon/daemon.go:4637 → :4636` (heartbeat jitter); `cmd/multica/cmd_repo.go:392 → :393-402` | No. What the daemon sends to the manager, and when, is unchanged. The heartbeat loop and the kill path are identical apart from the jitter source (`daemon/daemon.go:4584-4710 → :4583-4709`, `:5952-5957 → :5951-5956`). `terminal_report_queue.go`, `wsrpc.go`, `environment_commands.go` and `localenv/` have no diff. | None. The pod image needs no Dockerfile change. |
| 8 | **Go directive 1.26.11 and chi 5.4.0** (7b8bd90c4, a52dfea49). | `server/go.mod:3 → :3`, `:12 → :12` | No. `golang:1.27.3-alpine` (`sm:Dockerfile.pool:15`) satisfies `go 1.26.11`, so `GOTOOLCHAIN=auto` downloads nothing. The fork's commit message ties the Go bump to a `MaxBytesReader` fix, and the daemon calls `MaxBytesReader` nowhere (`git grep`), so the pod's Go patch level does not matter for it. chi routing does not change (see Method). | The pod build's `go mod download` now fetches chi v5.4.0. The build needs a module proxy that has it. |
| 9 | **The PAT `last_used_at` update runs on `context.WithoutCancel(r.Context())` instead of `context.Background()`** (b2c357f9e). | `middleware/daemon_auth.go:307 → :307`, `middleware/auth.go:255 → :255`, `cmd/server/router.go:2906 → :2906` | No. Neither context is ever cancelled, so the update outlives the request as before. | None. |

### For Phase B

- **Which backend image.** The fork's server-side changes that reach the manager are rows 1 to 5 and 9, and none of them changes what the manager sees. So the ghcr `multica-backend:v0.11.0` image is a faithful stand-in for the company server, as far as the manager is concerned. Building `multica-backend` from the fork's `Dockerfile` would test the exact server instead. It runs under the existing dev manifest unchanged (row 4).
- **Pod image build context.** `--build-context multica=../multica` builds from the working tree, and another agent edits that checkout. It was clean at 738d38669 when I read it. Before building the pod image, confirm `git -C ../multica status` is clean and `HEAD` is 738d38669, or build from a `git worktree` or `git archive` of `security/v0.11.0`.

## Checked and judged irrelevant

- **Frontend and JS dependencies.** This covers `apps/`, `packages/`, `package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `.npmrc` and `.nvmrc` (17071ef12, 936c1c78a, 62a7b52a4, the npm part of 7b8bd90c4, and the frontend parts of a203c3973, 484d382d0 and 738d38669). The manager never talks to the web app, and `Dockerfile.pool` builds nothing from the JS workspace. One side effect: the web app renamed its session storage slot, so browsers sign in again once.
- **Helm chart** (eea40dab4, 484d382d0, 46e12faae): security contexts, a read-only root filesystem, and the default Secret name moved into a helper. The dev namespace uses its own manifests (`sm:deploy/k8s/multica`).
- **Self-host tooling** (1a6c3af37, a203c3973): `.env.example`, `docker-compose*.yml`, `scripts/*.sh`, CI workflows, `scripts/credscan/` (its own Go module, not copied by `Dockerfile.pool`), and `Makefile`.
- **Test-only commits and files** (f61f6052b, 47cbec380, 35ad487b7, every `*_test.go`): fixtures split into pieces, and database tests skipped without `DATABASE_URL`.
- **Integrations and outbound clients.** This covers lark, slack, telegram, wecom, weixin, dingtalk, composio, the GitHub app client, the ClawHub skill search, remotemcp OAuth and the maintenance command. Only identifier renames and URL building changed. The renamed untagged OAuth structs are not JSON-encoded. SMTP now requires TLS 1.2 (`service/email.go:133`, 21ad1d87d).
- **Server hardening off the manager's routes.**
  - Page limits and offsets in autopilot, webhook_delivery, connected_apps, search_index, chat, chat_history, comment, activity and plugin_action are clamped to int32.
  - The issue-environment port is clamped, and GitHub issue numbers are parsed as 32-bit.
  - Two hash computations keep their values: the realtime shard and wecom queue (`util.SatInt32(h % n)`), and the seat-capacity lock key (same bits, read through `binary.Read`).
  - selfhosttelemetry, storage and awakeclock conversions are clamped. Jitter in dbstartup, channel engine and ghsnapshot now uses crypto/rand.
  - The scheduler heartbeat and the channel router contexts now derive from `WithoutCancel(ctx)`.
- **Backfill commands** refuse to run without `DATABASE_URL`, like the migrator (row 2).
- **`publicapi/v1/openapi.yaml`** renames the security scheme `pluginBearer` to `pluginAuth`. It is a document-local name for the plugin API, which the manager does not use.
- **Line citations in the compat report.** All the files it cites are byte-identical at the fork, or changed with equal line counts, except the two shifts named in the verdict. `server/cmd/server/runtime_sweeper.go`, `service/reclaim_check_cache.go`, `pkg/db/queries/agent.sql`, `handler/runtime.go`, `pkg/protocol/{messages,events,claim}.go`, `handler/daemon_workspace.go` and `handler/runtime_models.go` show no diff.

## Could not determine

- **The company's server settings.** Whether production sets `APP_ENV=production` with a strong `JWT_SECRET`, and whether a proxy in front of Multica adds an `Origin` header to daemon WebSocket upgrades. Such a proxy would lock out members' own laptop daemons under row 1, but not the manager.
- **How the company builds the server image.** The fork's `Dockerfile` uses `golang:1.27.4-alpine` at both refs. I could not read the GO-2026-6859 advisory, so I do not know which 1.27.x patch carries the fix. This does not matter for the pod daemon (row 8).
- **Anything at run time.** Every "No" above is read from source. Phase B's smoke and scenario runs against a v0.11.0 backend and a pod image built from 738d38669 are still the proof.
- **Frontend changes beyond file names and commit messages.** I did not read them, because the manager has no contact with the web app.
