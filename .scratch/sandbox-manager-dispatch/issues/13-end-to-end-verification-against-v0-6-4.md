# 13: End-to-end verification against v0.6.4

**What to build:** Show that the whole pool works against the server version the company runs. The compose end-to-end scenarios run against a Multica v0.6.4 server with one bot account in two workspaces. They cover:

- pod registration through the entrypoint;
- a single daemon identity;
- global queue order and quota;
- affinity reuse within hold;
- hold expiry;
- eviction under capacity pressure;
- maximum lifetime;
- reset leaving the skip directories in place.

The POOL and deployment documentation is updated to match.

**Blocked by:** 04, 06, 11, 12, 14

**Status:** resolved

- [x] Every scenario above passes against a v0.6.4 server with the fake agent
- [x] No call the manager makes is missing or behaves differently on v0.6.4
- [x] The manager and pod documentation describe the pod token, global queue, bindings, hold, eviction and reset as built, with no leftover LiteLLM, gateway or source-IP material

## Comments

Verified on branch `worktree-agent-a71849c17ebb04bb8` (2026-11-14), based on `feat/sandbox-manager-dispatch` at `ddcd9d9fb`:

- Driver: `deploy/sandbox/pool/compose/smoke/scenarios.sh` (+ `scenarios.yaml`; the shared setup moved into `lib.sh`, which `smoke.sh` now uses too). Backend `ghcr.io/multica-ai/multica-backend:v0.6.4` (pulled) with its own `pgvector/pgvector:pg17`, one bot account owning two workspaces (`pool-a`, `pool-b`), the manager and two fake pods of one slot each (`SANDBOX_POD_CONCURRENCY=1`, sync 2s, drain grace 10s, hold 0 by default). Hold, lifetime and quota come from the overrides file, which the driver rewrites between scenarios, so overrides and their hot reload are exercised too. Project `t13scen`, torn down with `down -v`; about 7 minutes. Final run: exit 0, every scenario PASS.
  1. Registration: both pods registered through the entrypoint, labelled with their container hostname, each with its own `mpt_` token in `.multica/config.json`. A heartbeat with no token, a forged `mpt_` token or a `mul_` token got 401. PASS.
  2. Identity: exactly one runtime in each workspace, both under the same daemon ID (`sandbox-manager-<hash>`); no pod ID or label in the runtime records. PASS.
  3. Reset: after a fake task and its reset, `/home/pod` held only `.multica` (the config with the same token, plus the daemon's own files), `.claude/skills/{hidden-runtime-skill,sandbox-environment}` and an empty `tmp`; no task file, `workspaces` or `.claude/projects`. The agent's comment listed both skills. PASS.
  4. Global queue: with both pods busy, tasks created a-low, a-high, b-urgent, b-high were leased b-urgent, a-high, b-high, a-low (priority, then age, across workspaces). PASS.
  5. Quota: with `pool-a: {max_leases: 1}`, pool-a's second task stayed queued while a pod was idle; pool-b's later task was leased first; pool-a's second task was leased after its first ended. PASS.
  6. Hold: with `hold_timeout: 30s`, a comment on the same issue ran on the held pod with no reset in between and found the first task's files; `hold_until` moved forward (renewal); the pod was then reset with `hold expired`. PASS.
  7. Eviction: with `hold_timeout: 45s` and both pods in hold (the second key went to the idle pod, not to the hold), a third key's task evicted the hold idle longest (`evicted for task <id> of another affinity key`) and ran there; the other pod kept its binding. PASS.
  8. Maximum lifetime: with `pool-b: {hold_timeout: 10m, max_lifetime: 15s}`, a held binding was reset at 15s (`maximum lifetime reached`). A 60s task was failed upstream after 15s + 10s grace with the lifetime error, and its pod reset (`maximum lifetime and drain grace passed`). PASS.
  - Upstream: the driver lists every request in the backend's log and fails on any 4xx or 5xx. All succeeded: `GET /api/me`, `GET /api/daemon/workspaces`, `GET /api/workspaces`, `POST /api/daemon/register`, `heartbeat`, `recover-orphans`, `GET .../tasks/pending`, `POST /api/daemon/tasks/claim`, `GET .../tasks/{id}/status`, `POST .../fail`, `GET /api/runtimes`, `PATCH /api/runtimes/{id}`, and the relayed `start`, `progress`, `session`, `complete`, `skill-bundles/resolve` and the agents' `mat_` comment calls. **No v0.6.4 incompatibility found; no manager code change was needed.**
- `smoke.sh` (now on `lib.sh`) against v0.6.4: PASS (project `t13smoke`, torn down).
- `go test -count=1 ./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/... ./cmd/pod-supervisor/` (uid 1000): 375 passed. `go vet` and `gofmt -l` clean on those packages.

Changes besides the driver:

- Fake agent: `SANDBOX_FAKE_SLEEP` (from the agent's custom environment) keeps a task running, to fill the pool. A comment-triggered task replies under its trigger comment (`--parent` from the prompt). Without that, v0.6.4 answered the fake's top-level comment with 409 ("comment-triggered tasks cannot create top-level comments"): a fake-agent issue, not a manager one.
- The smoke no longer creates `pool/compose/overrides/` in the checkout: `lib.sh` points `OVERRIDES_DIR` into its state directory.
- Docs: POOL.md gains "Bindings, hold and eviction", "Global queue and quota" and "Multica server version". README, GITOPS (overrides ConfigMap), SANDBOX.md (the last source-address phrasing), and the compose and k8s READMEs are updated. A grep for LiteLLM, gateway, source IP, nonce and SNAT under `deploy/sandbox` and in the manager and supervisor code finds only sentences saying that identity does not depend on the address.

Not verified: manager restart and pod loss are not in this driver (ticket 10 checked the restart by hand on `pooldev`); the bot as an invited member rather than the workspaces' owner; the k8s manifests on a cluster.
