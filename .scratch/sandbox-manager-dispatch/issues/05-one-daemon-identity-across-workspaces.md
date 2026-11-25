# 05: One daemon identity across workspaces

**What to build:** To the server, the manager is one machine, whichever workspaces its bot account serves. It uses a single daemon identity in every workspace. By default it is derived from the Multica URL and the bot account, and an environment variable can override it. This replaces the per-workspace derivation from the URL and workspace ID.

Runtimes stay per workspace and per agent CLI, because the server stores them that way, and are shared by every pod in that workspace. The runtime name keeps coming from its environment variable. Pods stay invisible to the server.

**Blocked by:** 03

**Status:** resolved

- [x] With two or more served workspaces, the server receives the same daemon identity in every registration, heartbeat and claim
- [x] Setting the daemon-identity environment variable changes it; leaving it unset gives the same derived value on every restart
- [x] In each workspace, every pod's registration is answered with that workspace's single runtime list
- [x] The server never receives a pod's own device name, daemon identity or address
- [x] The runtime name the server records equals the configured name

## Comments

**Verified** (from `server/`, as uid 1000): `go build ./...`; `go vet` and `gofmt -l` clean on `./internal/sandboxmanager/ ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/`; `go test -count=1` on those four packages: 275 passed; `go test -race -count=1 ./internal/sandboxmanager/`: 222 passed.

What changed (Upstream module and config only):

- `upstreamClient.identity` returns the one daemon_id. `PoolConfig.DaemonID` (env `SANDBOX_MANAGER_DAEMON_ID`) overrides it. Otherwise it is `sandbox-manager-` + sha256(Multica URL, NUL, account ID)[:24], with the account ID from `GET /api/me`, which accepts a PAT on v0.6.4 under `middleware.Auth`. It is resolved once per process on the first Sync. A failed lookup fails that Sync, so `runPool` stops at startup as it does for a bad PAT.
- `Pool.listWorkspaces` puts that ID in every discovered `WorkspaceConfig.ManagerID`, so registration and claim use it in every workspace (coordinator decision 6).
- Registration sends the former per-workspace ID (`sandbox-broker-…`) as `legacy_daemon_ids`, so v0.6.4 merges the old runtime rows and agents stay bound.
- The static workspace-list mode (removed by ticket 12) keeps its per-workspace derivation. `DaemonID` without `Token` is refused.
- `deploy/sandbox/POOL.md` documents the variable and replaces the "one Multica per pool" limit.

Tests: `server/internal/sandboxmanager/daemon_identity_test.go` (the fake server `syncMultica` now answers `/api/me` and records every request) and `TestLoadPoolReadsTheDaemonIdentity` in `cmd/sandbox-manager`.

- Box 1: registrations and claims in two workspaces carry the same daemon_id. The heartbeat body on v0.6.4 is `{runtime_id}` only and carries no daemon_id. It targets the runtimes registered under that ID, and the test checks it carries nothing of the pod.
- Box 2: override test. A restart (two managers, same server and account) gives the same derived value; another account gives another.
- Box 3: two pods get the same runtime list per workspace (existing behaviour, now asserted).
- Box 4: no request body or header the server receives contains either pod's name, daemon_id, IP or boot nonce. device_name is the configured name.
- Box 5: with one provider, the registered runtime name equals `SANDBOX_MANAGER_NAME`. With several providers the name gets a ` (provider)` suffix, as before.

**Not verified:** no compose smoke against a real v0.6.4 server. The `legacy_daemon_ids` merge and `/api/me` with a PAT were checked by reading v0.6.4 source only.
