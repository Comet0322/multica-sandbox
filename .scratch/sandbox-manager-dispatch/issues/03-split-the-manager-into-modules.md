# 03: Split the manager into modules

**What to build:** A prefactor that changes no behaviour. Today every responsibility of the manager hangs off one pool object, with one lock and shared state. Split it into the modules the later tickets change independently:

- **Upstream**: talks to the Multica server.
- **Pod API**: plays the server toward pods, including the wake socket and the control channel.
- **Pod registry**: pod identity. It still uses source IP in this ticket.
- **Dispatcher**: the loop that wires the modules together and carries out decisions.
- **Workspace sync & config**: discovery, drain, configuration and reload.
- **Audit / inventory**: the audit log and the admin API.

Placement, lifetime and claim-selection logic stays where it is, behind the Dispatcher, until ticket 07 replaces it.

**Blocked by:** 02

**Status:** resolved

- [x] Each module has a small interface. Only the Dispatcher knows the other modules
- [x] Upstream, Pod API and Pod registry no longer share the pool's lock or fields
- [x] Every test carried over in 01 and kept in 02 passes unchanged, apart from constructor wiring
- [x] Manager configuration, environment variables, HTTP routes and audit records are unchanged

## Comments

**Verified** (from `server/`, as uid 1000, after merging the integration tip that brought ticket 06):

- `go build ./...` passes.
- `go vet` passes on `./internal/sandboxmanager/ ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/`.
- `go test -count=1` on those four packages: 267 passed. `go test -race -count=1 ./internal/sandboxmanager/`: 215 passed.
- Test files differ from the integration tip only in wiring: `export_test.go` (`SetNow` sets the shared clock) and the two white-box wake tests (`p.wakers` / `p.wakePod` / `p.notifyWorkspacesChanged` became `p.api.…`). No external (`sandboxmanager_test`) test changed.
- `server/cmd/sandbox-manager` is untouched: `PoolConfig`, the env vars, `NewPool`, `PodHandler`, `AdminHandler`, `Run`, `Sync`, `SyncLoop` and `Reload` keep their signatures. The pod route switch and the admin routes moved verbatim; audit records are built by the same helpers.

**Not verified:** the compose smoke was not run.

**Merge with 06:** `pool_reset.go` conflicted. The done report with its `failed` paths now lives in the dispatcher's `controlReport` (`controlReport.Failed`); the pod API only decodes it.

**Module map** (all in `server/internal/sandboxmanager`):

| File | Module | Interface |
|---|---|---|
| `pool.go`, `pool_claim.go`, `pool_dispatch.go`, `pool_reconcile.go`, `pool_reload.go` (drain), `pool_sync.go` (Sync orchestration), `pool_reset.go`, `pool_placement.go`, `pool_lifetime.go`, `workspace.go` | **Dispatcher** = `*Pool` | Public API unchanged. It implements `podBackend` for the Pod API and `adminSource` for the admin API. It owns `mu`, the leases, the waiting and forced tasks, the fail-retry queue, and the live fields of pod and workspace records. Placement, lifetime and claim selection (`placeLocked`, `expireSharedLocked`, `claimTargetsLocked`, `reserveClaim`) are still here for ticket 07. |
| `upstream_client.go` (+ `upstream.go` connection, `rewriteRegistration`) | **Upstream** = `upstreamClient` | `connect(ticket)`, `adoptHeaders`, `listWorkspaces`, `register`, `recoverOrphans`, `heartbeatDue`/`heartbeat`, `claim`, `fail`, `taskStatus`, `runtimeVisibilities`/`setVisibility`, `forward` (reverse proxy). Own lock: identity headers and heartbeat throttle. Ticket 05 changes the daemon_id/registration here and in `WorkspaceConfig.normalize`. Ticket 08 adds the pending list here. |
| `podapi.go`, `pool_wake.go`, `pool_models.go` | **Pod API** = `podAPI` | HTTP toward pods: routing, decoding, register/heartbeat/control/claim/task/repos/runtime routes, wake socket, model-request hand-off. It asks the Dispatcher through `podBackend` (13 methods, keyed by source IP) and forwards through an injected `forward`. Own locks: `wakeMu`, `modelMu`, `notifyMu`. Called by the Dispatcher: `wakePod`, `wakeable`, `notifyWorkspacesChanged{,Limited,Now}`, `noteModelRequest`. For ticket 04: `sourceIP` and the SNAT check live here; swap them for token resolution. |
| `registry.go` | **Pod registry** = `podRegistry` | `get/set/remove/current/all/sorted`, `prune`, `pruned`, `announce`/`known`. Still keyed by source IP. Own lock over the index only; `pod` (struct still in `pool.go`) lending fields are guarded by the Dispatcher's `mu`. |
| `workspaces.go` (+ `PoolConfig`/`normalize` in `pool.go`, `normalizeWorkspaces` in `pool_reload.go`) | **Workspace sync & config** = `workspaceSet` | `served`, `get`, `drainingList`, `apply` (reload diff, called under the Dispatcher's `mu`), `finishDrain`, `debounce`, `recordSync`/`syncStatus`/`notReady`. Own lock over the index; workspace records' live fields are guarded by the Dispatcher's `mu`. |
| `audit.go`, `admin.go` | **Audit / inventory** | `auditLog.record` (stamps from the shared clock). `newAdminHandler(adminSource)` serves `/v1/inventory` and `/v1/leases`. The `Inventory`/`PodInfo` types are in `admin.go`. |
| `clock.go` | shared clock | `now`/`set`, injected into the modules. |

Lock order: the Dispatcher's `mu` first, then any module lock. No module calls another module, and none calls back into the Dispatcher while it holds its own lock.
