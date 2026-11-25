# 12: Remove legacy single-workspace config

**What to build:** The manager runs only under one bot account, which discovers its own workspaces. Remove the legacy single-workspace and explicit workspace-list configuration path, following the rule against compatibility shims. Make naming consistent where fields were renamed from Token to Ticket but their comments still say Token.

**Blocked by:** 05

**Status:** resolved

- [x] The manager accepts only the bot-account configuration; the legacy settings are gone, and the documentation no longer mentions them
- [x] Field names and their comments agree
- [x] All tests pass

## Comments

**Verified** (from `server/`, as uid 1000): `go build ./...`; `go vet` and `gofmt -l` clean on `./internal/sandboxmanager/ ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/`; `go test -count=1` on those four packages: 262 passed; `go test -race -count=1 ./internal/sandboxmanager/`: 220 passed.

What changed (`server/internal/sandboxmanager` only; the command already accepted only `SANDBOX_POOL_TOKEN`):

- `PoolConfig` lost `Workspaces`, `MulticaTicket`, `WorkspaceID` and `ManagerID`. `NewPool` refuses a config without `Ticket` ("the bot account's ticket is required"). `WorkspaceConfig` lost `Ticket` and `ManagerID`: every workspace acts with the pool's ticket and claims and registers under `upstreamClient.knownIdentity()` (resolved by Sync before any workspace is served). The per-workspace derivation survives only as the `legacy_daemon_ids` value.
- `runtimeMode` is always `local`; the `Ticket == ""` branches in Sync, SyncLoop, readiness and visibility are gone.
- `Pool.Reload` became `reload`, fed only by Sync. Tests reach it through `export_test.go`, which resolves the identity first, to stage workspaces with different settings (lease cap, placement, runtime name) until ticket 11 brings per-workspace overrides.
- Comments that still said Token/MulticaToken/per-workspace PAT now say ticket / bot account (`PoolConfig.Ticket`, `DaemonID`, `proxyRoute.ticket`, `runtimeTicket`, discovery and reload docs).

Tests (coordinator decision 7): no test of a remaining behaviour was deleted.

- `fakeMultica` (harness) answers `/api/me`, `/api/daemon/workspaces` (and `/api/workspaces`), `GET /api/runtimes`, `PATCH /api/runtimes/runtime-a` and `GET /api/daemon/runtimes/runtime-a/tasks/pending`. `newHarness` builds the pool with `Ticket` and runs one Sync.
- `multiMultica` serves one bot PAT (`patPool`) and tells workspaces apart by `X-Workspace-ID`, the workspace in the path or body, the runtime, or the task it handed out. It also lists the workspaces, answers `/api/me`, runtime visibility and the pending list. `newMultiHarness`/`newReloadHarness` share `newBotHarness`.
- Tests of the removed static mode were turned into bot-account ones: `TestPoolTokenCannotBeCombinedWithWorkspaceSettings` and `TestSyncNeedsAToken` -> `TestPoolRunsOnlyUnderTheBotAccount` (the red test); `TestStaticWorkspacesStayCloudAndAreNeverPatched` -> `TestBotAccountPoolRegistersLocalRuntimesAndSharesThem`; `TestReadyzIsAlwaysOKWithoutDiscovery` -> `TestReadyzIsOKAfterTheFirstSync`. `TestSyncLoopReturnsAtOnceWithoutAToken` was removed: a pool without a ticket can no longer be built. The "missing PAT" invalid-entry cases went with the field; the reload restart-flag case now renames the runtime instead of rotating a per-workspace PAT.
- `TestDroppedRegistrationTellsPodsToLookAgainOnceInAWhile` advances its clock past the 30 s hint limit first, because the harness's first Sync now sends the workspaces_changed hint.

Docs: `deploy/sandbox/*.md`, compose and k8s manifests already named only the bot-account settings; a grep for the legacy names (`WORKSPACE_ID`, `MULTICA_TOKEN`, `SANDBOX_POOL_WORKSPACES`, `MANAGER_ID`, workspace list files) found nothing to remove.

**Not verified:** no compose smoke against a real v0.6.4 server.
