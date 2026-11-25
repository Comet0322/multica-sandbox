Status: Phase A done (read only, 2026-11-26)

# Multica v0.11.0 compatibility: Phase A report

Goal: the company runs Multica v0.11.0 and the manager targets v0.6.4. This report lists what changed between the two tags that the manager touches, whether it breaks, and the fix.

**Verdict, from the source only.** Nothing the manager sends or answers breaks against v0.11.0. The wire types in `daemonwire` and their fixtures still match. The pod image needs no Dockerfile change. Issue 01's conclusions hold, with two invariants added. Three decisions remain for Phase B: what to do with the new environment reports, whether to keep opencode on its new ACP transport, and the doc and tag updates. Nothing here was built or run. Phase B has to prove all of it.

Method: `git show <tag>:<path>`, `git diff v0.6.4 v0.11.0`, `git grep` in `../multica`. Two delegates read the server-behaviour and pod-image parts. I re-checked their load-bearing claims myself: the ownership checks, the claim handler, the heartbeat, the reclaim gate, the opencode switch and the new routes. In the citations, `m:` is `../multica` and `sm:` is this repo. `a → b` gives the line at v0.6.4, then at v0.11.0.

## 1. Daemon protocol toward Multica (the manager as the bot's daemon)

| # | What changed | Manager breaks? | Fix | Issue 01 |
| --- | --- | --- | --- | --- |
| P1 | **Runtime ownership on every daemon route (MUL-8154).** At v0.6.4 the runtime and task checks are workspace access only (`m:server/internal/handler/daemon.go:91-160` at v0.6.4). At v0.11.0 a user credential must be the runtime's owner (`daemon.go:132-148`). Runtime routes answer 404 "runtime not found" (`:184-203`), heartbeat included (`:1571`). Task routes answer 404 "task not found" (`:274-291`), and so does GetTaskStatus (`:5770-5775`). The batch claim silently skips unauthorized runtimes (`:2251-2257`). | No. Every upstream call and every relayed pod report goes with the bot PAT (`sm:internal/multica/conn.go:122`, `client.go:189-191`, `pool/pool.go:396`), and that account registered, so owns, the runtimes. It breaks only if the PAT is replaced by another account's. Then heartbeats get 404 "runtime not found", the manager drops the registration (`runtimes.go:264-277`), re-registration gets 403 (P2), claims come back empty, and pods' terminal reports get 404 "task not found", which ends their leases (`pool.go:404-410`) and makes the stock daemon kill the agent. | None in code. Document: rotating the PAT must keep the same account. | Holds. New invariant: both replicas use the same bot account. |
| P2 | **Registration fenced by owner.** It is serialized per (workspace, daemon_id) (`m:daemon.go:750` at v0.11.0). It is refused 403 when any existing runtime under that daemon_id has another owner or no owner (`:760-783`). A legacy-id merge runs only when the caller owns both rows (`:1152-1167`). None of this exists at v0.6.4 (`daemon.go:402-741`). | No. The same PAT registers the same daemon_id (`sm:runtimes.go:248-257`). The `sandbox-broker-` legacy rows merge as before if the same account made them. | None. If the logs show 403 at registration, look for ownerless rows under the manager's daemon_id. I could not check the company database. | Holds. New invariant: both replicas register under the same daemon_id. It is derived from the server URL string and the account (`sm:client.go:64-86`), so a replica given a different spelling of the URL claims as another machine. For HA, set `SANDBOX_MANAGER_DAEMON_ID` explicitly (`sm:cmd/sandbox-manager/run.go:146`). |
| P3 | **Batch claim gains `force_recheck_runtime_ids`.** The request adds the field and the answer adds `force_rechecked_runtime_ids` (`m:daemon.go:2133-2175` at v0.11.0, vs `daemon.go:1705-1715` at v0.6.4). The answer carries it whenever the request has the `force-recheck-v1` capability header (`:2168`). The manager replays that header from the first pod (`sm:client.go:119-126`, `conn.go:116-118`). | No. `Conn.Claim` decodes only `tasks` (`sm:conn.go:188-193`). | Optional. The empty-claim cache exists at both tags (`m:service/task.go:3719` → `:3986`). The manager could force a recheck of a runtime whose pending list shows a queued task that a claim just returned nothing for. | n/a |
| P4 | **Per-runtime concurrency cap.** v0.11.0 adds `agent_runtime.max_concurrent_tasks`, checked on every claim (`m:service/task.go:3806-3825`) and settable through `PATCH /api/runtimes/{id}` (`handler/runtime.go:489-512`). There is no runtime-level cap at v0.6.4; that tag's `max_concurrent_tasks` is per agent. | Not by default: the cap is NULL. If an admin sets it on a manager runtime, every pod together is capped at that number, and the claims come back empty without an error. | Document it. Optionally flag "pending shows queued, claim empty" on the dispatch page. | n/a |
| P5 | **Runtime visibility.** `PATCH /api/runtimes/{id}` keeps `visibility` (`m:handler/runtime.go:467` at both tags). It adds `shared_with` (`:469`, response `:47`) and an owner-only check (`:531`, `:577`). `ListAgentRuntimes` is unchanged (`runtime.go:1227-1300` → `:1244-1317`, identical text). | No. The manager sets `public` as the owner (`sm:runtimes.go:406-415`). | None. | n/a |
| P6 | **Retries follow the agent's current runtime.** At v0.11.0 a retry is stamped `COALESCE(a.runtime_id, p.runtime_id)` (`m:pkg/db/queries/agent.sql:570`, `:600`). At v0.6.4 it took the parent task's runtime (`agent.sql:513`). | No. An agent bound to a manager runtime retries there. | None. | Holds. A retry after a `timeout` or `runtime_recovery` lands on the agent's current runtime. |
| P7 | **Task messages take an `idempotency_key`.** New at v0.11.0 (`m:daemon.go:5885-5886`, row id derived at `:5959-5960`), not present at v0.6.4. The stock daemon does not set it: there is no match under `server/internal/daemon` at v0.11.0. | No. | Optional. See the issue 01 column. | Opportunity: the manager could buffer pod messages across a failover and replay them with a key. That would close the one loss issue 01 accepts. |

**Checked and unchanged**, so the manager's calls and `daemonwire` fixtures hold:

- **Routes.** The `/api/daemon` routes (`m:cmd/server/router.go:1559-1613` → `:1677-1737`) only gain `:1709-1710` and `:1721-1724`.
- **Auth and identity.** `DaemonAuth` still takes `mul_` PATs (`m:middleware/daemon_auth.go:268-309` at v0.11.0). `/api/tokens/current/renew` still exists (`router.go:1919` → `:2086`). `GetMe` is `handler/auth.go:483` → `:498`.
- **Registration.** The request struct is `daemon.go:193-200` → `:330-358`, with `legacy_daemon_ids` kept. The answer's `runtimes` is `:738-741` → `:1125-1134`; it adds `settings` and `ca_certs`. The runtime fields `id`, `daemon_id`, `provider`, `owner_id` and `visibility` are at `handler/runtime.go:28-46` at both tags.
- **Claimed task.** The fields the manager reads (`sm:task.go:13-29`) are at `m:handler/agent.go:375-379`, `508`, `516`, `552`, `559`, `598` → `:408-412`, `558`, `570`, `612`, `619`, `662`.
- **Pending list.** The query is `agent.sql:2109-2112` → `:2250-2253`, identical. The handler is `daemon.go:4092-4113` → `:4722-4743`, identical.
- **Heartbeat.** The ack is `protocol.DaemonHeartbeatAckPayload`, `pkg/protocol/messages.go:417` → `:479`, identical. Its request still has `runtime_id` (`daemon.go:1022` → `:1444`).
- **Fail and status.** The fail body still uses `error` (`daemon.go:5782-5783` at v0.11.0), and GetTaskStatus still answers `{"status"}` (`:5778`).
- **Other files.** `handler/daemon_workspace.go`, `handler/runtime_models.go` and `pkg/protocol/claim.go` show no diff. The workspace list keeps `id` and `slug` (`handler/workspace.go:98,100` → `:99,101`).
- **WebSocket events.** `daemon:task_available` and `daemon:workspaces_changed`, and `TaskAvailablePayload`, are untouched by the `events.go` and `messages.go` diffs.

## 2. What the v0.11.0 pod daemon calls or expects of the manager

| # | What changed | Manager breaks? | Fix | Issue 01 |
| --- | --- | --- | --- | --- |
| D1 | **New `POST /api/daemon/tasks/{id}/environments` and `/environment-configs`** (`m:internal/daemon/client.go:733-752`, `router.go:1721-1722` at v0.11.0). The body says `"controllable": true` (`client.go:737-740`): the member's stop, restart and log buttons are carried out by this daemon. Those commands reach a daemon only through a `daemon:pending_work` hint of kind `environment_command` (`daemon.go:4784-4788`). The URLs are `http://localhost:<port>` (`localenv/detect.go:168`, `localenv/config.go:134`). | No error. The manager relays both with the PAT (`sm:daemonapi.go:311-312`, `pool.go:392-418`). But the manager never sends that hint (`sm:wake.go:41-54`) and refuses the command claim (D3). So the sidebar offers buttons that never act and links to the member's own localhost, for a pod deleted after its hold. | **Decision.** I recommend answering both locally with 200 and not relaying them, as the manager already does for `update` and `local-skills` (`sm:daemonapi.go:361-363`). | n/a |
| D2 | **New `POST tasks/{id}/context-receipt` and `/rate-limits`** (`m:client.go:757-770`, `router.go:1723-1724`). Rate limits are kept as the state of the task's runtime (`client.go:763-765`). | No. They are relayed like any task report. All pods share one runtime, so the runtime shows the latest pod's snapshot. | None. | n/a |
| D3 | **New `POST runtimes/{id}/environment-commands/claim` and `/{cmd}/result`** (`m:client.go:793-805`, `router.go:1709-1710`). They are called only on the hint in D1 (`daemon.go:4784-4788`, `environment_commands.go:36-70`). | No. The manager refuses them with 403 (`sm:daemonapi.go:364-365`); the daemon logs a warning and carries on (`m:environment_commands.go:53-56`). With no hint, they are never called. | None. Goes with D1. | n/a |
| D4 | **The runtime-profiles call adds `?daemon_id=`** (`m:client.go:1300-1302` at v0.11.0, no query at v0.6.4). | No. The manager matches on `r.URL.Path` (`sm:daemonapi.go:290`). The answer keys `workspace_id` and `runtime_profiles` are unchanged (`m:client.go:1291-1294`). | None. | n/a |
| D5 | **Force-recheck hints.** The daemon records a woken runtime only for a `task_available` hint that carries a `runtime_id` (`m:wakeup.go:409`, `:496-522` at v0.11.0). It treats a claim answer without `force_rechecked_runtime_ids` as an older server (`wsrpc.go:312-334`). | No. The manager's hint has an empty payload (`sm:wake.go:41-46`) and its claim answer has only `tasks` (`daemonapi.go:283-289`). | None. | n/a |
| D6 | **Capabilities.** The daemon adds `force-recheck-v1`, `workflow-status-rules-v1` and `cli-comment-steer-v1`. It drops `execution-manifest-v1`, `agent-skill-v1` and `remote-mcp-v1` (`m:client.go:204-218` diff). The manager adopts the first pod's `X-Client-*` headers for the process's lifetime (`sm:client.go:119-126`, `daemonapi.go:227-232`). | Not with one image. A manager that started before a pod image roll keeps claiming with the old daemon's capabilities until it restarts. | Restart the manager after changing the pod image, or adopt the headers of the newest pod. Low priority. | n/a |

**Checked and unchanged**:

- **Wake socket.** Frame handling is the same (`m:wakeup.go:398-455` at both tags; the diff only adds the woken-runtime note). The manager never acks WebSocket heartbeats, so `rpc-v1` is never negotiated and the daemon stays on HTTP, as at v0.6.4.
- **Not-found detectors** for "task not found", "runtime not found" and "workspace not found" (`client.go:37-93` → `:40-94`).
- **Registration answer.** The daemon only gains `ca_certs` (`client.go:1237`).
- **Task-token paths.** `plugin-hooks` and `plugin-mcp/{id}/credential` are unchanged (`router.go:1573-1576` → `:1691-1694`), so `TaskTokenPath` holds (`sm:task.go:96-102`).
- **Agent CLI calls.** The CLI's calls to `/api/workspaces` are unchanged (`m:cmd/multica/cmd_login.go:111,195`, `cmd_workspace.go:234,346` at both tags).

## 3. Server and daemon behaviour issue 01 relies on

| # | Fact in issue 01 | v0.11.0 | Issue 01 |
| --- | --- | --- | --- |
| F1 | Offline 150 to 180 s after the last heartbeat; 3 h reconnect grace | Unchanged. `runtime_sweeper.go` is identical. The 150 s comes from `service/task.go:192` → `:193`, and Redis liveness is still 90 s (`handler/daemon.go:1063` → `:1485`). The grace can be overridden by `MULTICA_RUNTIME_RECONNECT_GRACE` at both tags (`cmd/server/main.go:740` → `:748`). | Holds. I could not read the company's setting. |
| F2 | A dispatched, unstarted task is handed again after 90 s; it fails as a retryable `timeout` at 300 s | Unchanged. The constants are `task.go:197-198` → `:198-199`; the query is `agent.sql:843-886` → `:950-994` (adds `shared_with` at `:982`). The Redis reclaim gate is identical (`service/reclaim_check_cache.go`). Its first hint fires at 90 + 5 s, with retries every 30 s. The 300 s timeout is `runtime_sweeper.go:74`. Retryable reasons are `task.go:5260-5261` → `:5726-5727`. | Holds, with a refinement: the reclaim also needs the 45 s prepare lease to be absent or expired (`agent.sql:858` at v0.6.4). It happens on a claim from that runtime about 95 s after dispatch. |
| F3 | No claim idempotency key; `dispatched_at` fences StartTask with 409; the daemon drops a 409 silently | Unchanged (`agent.sql:964-970` → `:1060-1066`; `daemon/client.go:519-521` → `:558-560`; `daemon.go:5800-5804` → `:6136-6141`). New: StartTask answers 409 "task claim is stale" before locking (`handler/daemon.go:4809-4811`). Process-cleanup 409s apply to pi, omp and museapp only (`:4819-4845`). | Holds. The manager passes the claimed task through unchanged (`sm:task.go:28`, `ParseClaimed`), so `dispatched_at` survives. |
| F4 | Heartbeats and claims are retried forever; complete and fail go to an on-disk outbox; progress and messages are not retried | Unchanged. The heartbeat loop is byte-identical (`daemon.go:4388-4514` → `:4584-4710`), and so is `terminal_report_queue.go`. Progress and messages still have no retry (`client.go:638-670`, `daemon.go:10427-10435`). The outbox is keyed by server URL, profile and daemon ID (`terminal_report_queue.go:98`). | Holds. A pod keeps its outbox as long as it lives. |
| F5 | The agent is killed only on 404 "task not found" or a terminal status; three rejections over 10 minutes drop a completion report | Unchanged (`daemon.go:5614-5619` → `:5952-5957`, `client.go:53-62` → `:56-65`, outbox `:30-31`, `:437-449`). Refinement: a 400 also counts toward the three. New: v0.11.0 also answers 404 "task not found" for an ownership mismatch (P1). | Holds while both replicas use the same account (P1). |
| F6 | recover-orphans fails every dispatched or running task of the runtime | Unchanged (`agent.sql:1196-1211` → `:1300-1315`). The handler now checks runtime ownership (P1). | Holds. Issue 01 already drops the calls. |

**Not read:** the server's WebSocket handshake changes (the manager uses HTTP only upstream) and the Redis half of the sweeper (in a file with no diff).

## 4. Pod image build (`deploy/Dockerfile.pod`)

| # | What changed | Breaks? | Fix |
| --- | --- | --- | --- |
| I1 | Server image tags: `multica-backend:v0.6.4` and `multica-web:v0.6.4` (`sm:deploy/k8s/multica/backend.yaml:38`, `web.yaml:37`, `deploy/k8s/dev/up.sh:29`). The image names are the same at v0.11.0 (`m:docker-compose.selfhost.yml:56`). | The dev and e2e namespace keeps running v0.6.4. | Change them to `v0.11.0` in Phase B. |
| I2 | **opencode moves to ACP.** At v0.11.0, a built-in opencode at 1.15.0 or newer runs as `opencode acp` (`m:server/pkg/agent/opencode.go:151`, `opencode_acp.go:95-126`). The file does not exist at v0.6.4. The pod installs `opencode-ai@1.19.5` (`sm:Dockerfile.pod:142`), and upstream verified 1.19.4 (`opencode_acp.go:33-34`). The `fake` target has no opencode, so e2e cannot catch a failure. | Unknown. | **Decision.** Run one real opencode task on the default target in Phase B. Pin `MULTICA_OPENCODE_TRANSPORT=run` in the ENV block (`sm:Dockerfile.pod:113-119`; the switch is read at `opencode_acp.go:70`) only if that task fails. |
| I3 | Go: `golang:1.27.3-alpine` (`sm:Dockerfile.pod:15`). `server/go.mod:3` says `go 1.26.9` at both tags, with no toolchain line. | No. | None. |
| I4 | **The build context is the working tree, not a tag.** `../multica` is on `security/v0.11.0` (`git describe`: `v0.11.0-19-g738d38669`). | No. The company's hardened branch is the right source. | State it in DEPLOYMENT.md, which says "at the version the server runs (v0.6.4)" (`docs/DEPLOYMENT.md:53-54`). |
| I5 | Daemon config, flags and env are unchanged. `daemon start --foreground`; `.multica/config.json`; health on 127.0.0.1:19514 `/health`; `MULTICA_WORKSPACES_ROOT`, `MULTICA_AGENT_TEMP_BASE`, `MULTICA_GC_ENABLED`, `MULTICA_DAEMON_AUTO_UPDATE`. The fake claude still gets its prompt as the first stdin line (handshake from 2.1.110 on, `m:pkg/agent/version.go:140` → `:146`). | No. | None. |

## 5. Phase B change list

Each item cites its row above.

1. Point the dev namespace at v0.11.0 (I1). Build the fake and default pod images from `../multica` as it is checked out (I4). Run `smoke.sh` and `scenarios.sh`.
2. D1: answer `tasks/{id}/environments` and `environment-configs` in `daemonapi.go`, with a test that they are not relayed. This is the only code change the reading calls for.
3. I2: one real opencode task on the default image; pin `MULTICA_OPENCODE_TRANSPORT=run` only if it fails.
4. Docs and comments that say v0.6.4:
   - `AGENTS.md:7,16`
   - `docs/ARCHITECTURE.md:169`
   - `docs/DEPLOYMENT.md:53-54,594,653`. Line 594's route list is still accurate for v0.11.0. Its phrase "no migration newer than" should just name v0.11.0.
   - `deploy/k8s/README.md:9`, `deploy/k8s/dev/up.sh:2`, `deploy/k8s/multica/kustomization.yaml:1`
   - `deploy/k8s/e2e/lib.sh:2`, `scenarios.sh:2-3`, `e2e/README.md:4-5`
   - `internal/multica/daemonwire/daemonwire.go:3`, `daemonwire_test.go:12`

   Keep the fixtures as they are: the types did not change. Optionally add a v0.11.0 heartbeat ack captured from the dev namespace.
5. For the two-services work (issue 01), add the invariants from P1 and P2: one bot account, and `SANDBOX_MANAGER_DAEMON_ID` set explicitly on both replicas. P7 is an optional way to stop losing messages during a failover.
6. Proposal on v0.6.4 support: drop it. Nothing above needs a version switch, so dropping it is a doc change, not a code removal.

## 6. Could not determine without running

- That v0.11.0 `go build ./cmd/multica` compiles in our builder, and that ghcr has `multica-backend` and `multica-web` at `v0.11.0`.
- Whether the v0.11.0 backend and web images run under the dev manifests' security settings.
- opencode 1.19.5 over ACP with our `opencode.json` (I2).
- The company's `MULTICA_RUNTIME_RECONNECT_GRACE`, whether anyone set `max_concurrent_tasks` on a manager runtime (P4), and whether ownerless runtime rows exist under the manager's daemon_id (P2).
- A full task through the manager with a v0.11.0 daemon and server. Every "No" in the tables above is read from source, not observed.
