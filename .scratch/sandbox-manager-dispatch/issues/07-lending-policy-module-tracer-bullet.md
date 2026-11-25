# 07: Lending policy module: tracer bullet

**What to build:** A pure, deterministic lending policy module with an injected clock and no network. Events go in, decisions come out. The Dispatcher carries out its decisions in place of today's placement, lifetime and claim-selection logic.

This first slice supports only:

- the `task` affinity rule;
- capacity measured in slots, with configurable pod capacity and each task needing 1;
- reserving before claiming, and releasing the reservation when the claim returns nothing;
- a hold timeout of 0, so the pod is reset as soon as its task ends.

The module's types already leave room for more resources, more affinity rules and binding lifecycle states.

**Blocked by:** 03

**Status:** resolved

- [x] The module emits claim, place, reset, fail and end-lease decisions from pod, claim, task-end, time and reset-done events
- [x] No claim is sent without a reservation; an empty claim releases its reservation
- [x] A pod with capacity for N slots runs at most N tasks at once
- [x] Table-driven tests cover these rules at the module seam with a fake clock
- [x] The harness's lease → reset → idle cycle passes through the module
- [x] The old placement, lifetime and claim-selection code paths are removed

## Comments

**Verified** (from `server/`, as uid 1000, after merging the integration tip that brought tickets 05, 12 and 14):

- `go build ./...`, `gofmt -l` (clean) and `go vet` on `./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/` pass.
- `go test -count=1` on those packages: 279 passed, 16 skipped. `go test -race -count=1 ./internal/sandboxmanager/...`: 237 passed.
- Claims use the single daemon identity from ticket 05 (`p.up.knownIdentity()` in `claimRound`).
- Module seam: `internal/sandboxmanager/lending/policy_test.go` holds table-driven scenarios with a fake clock. They cover the lease/reset/idle cycle, reservations granted, used and released, failed-claim backoff, N-slot capacity (through a test-registered shared rule), the most recently used binding, no initiator, quota and rotation, lost, interrupted and replaced pods, and the displaced-task wait.
- HTTP seam: the existing harness tests (claim, wake, reset, reconcile, stock daemon, multi-workspace quota) now run through the module without changes.

**Not verified:** the compose smoke was not run.

**Deliberate regressions until later tickets.**

- Only the `task` rule exists, so `SANDBOX_PLACEMENT=issue|chat` (pool or workspace) is refused at start.
- Sixteen shared-placement and lifetime tests carry `t.Skip("ticket 09: …")` or `t.Skip("ticket 10: …")`. Re-enable or rewrite them there. Three of them (`TestFullBoundPodHands…`, `TestTaskForAnotherKeyWaits…`, `TestTaskWaitingPastTheLimit…`) describe tasks waiting in the manager, which reserve-then-claim removes; rewrite them rather than revive them.
- `SANDBOX_POD_IDLE_TIMEOUT`, `_MAX_LIFETIME` and `_DRAIN_GRACE` are still parsed but not applied.
- `TestReconcileAsksAtMostEightAtOnce` now uses 20 pods with one task each. `TestDispatchWakesTheTargetPodWhenTaskIsPlacedElsewhere` uses the task rule.
- The executor (`placeClaimed`) still refuses tasks with no initiator; ticket 09 removes that.

**Module API** (`server/internal/sandboxmanager/lending`). It is pure, has no I/O and is not goroutine-safe; the Dispatcher calls it under `Pool.mu`.

- `New(Config{Now, Capacity Resources, Rule, Rules Registry, Backoff, PlacementWait})`. `Resources{Slots}` is the capacity and demand vector (`TaskDemand` is 1 slot). CPU and memory fields go there.
- Workspace events: `SetWorkspace(id, WorkspaceSettings{Quota, Rule})` adds or changes a workspace and applies to new claims and placements. `RemoveWorkspace(id)` stops claiming from it. Ticket 11 feeds overrides through `SetWorkspace`.
- Pod events: `PodRegistered(pod, ws, needsReset)`, `PodLost(pod, failReason, endReason)`, `PodInterrupted(...)` (a supervisor event: fail the started tasks, reset the pod), `PodReplaced(...)` (the old instance is void and the pod forgotten; also used when a lost pod is pruned), `ResetDone(pod)`.
- Claim events: `RequestClaim(ClaimRequest{Pods, Workspaces, Exclude})` returns at most one `Claim{Workspace, Reservation, Pod}`. `ClaimReturned(res, *Task|nil, failed)` returns `Place`, plus `Release` when an existing binding took the task, or `Release` alone.
- Task events: `TaskStarted(id)` (the pod picked it up). `TaskEnded(id, reason)` returns `EndLease`, plus `Reset` when the binding is left empty (hold timeout 0). `Tick()` lets time pass: displaced tasks past `PlacementWait` get `Fail`. Query: `Busy(ws)`.
- Decisions: `Claim`, `Release`, `Place{Task, Workspace, Pod, Binding, NewBinding}`, `Reset{Pod, Reason, Task}`, `Fail{Task, Workspace, Reason}`, `EndLease{Task, Reason}`. Each has a `String()` that the scenario tables compare.

**Hooks for the next tickets.**

- 08: claim selection is `pickWorkspace`: rotation after `lastWorkspace`, quota as placed tasks plus reservations, and backoff. Replace it with the global queue, and once holds exist, count only running tasks against quota. The executor side is `claimRound` / `claimTargetsLocked` in `pool_lending.go`.
- 09: register rules in `Builtin()` (`affinity.go`). `affinityKey` already prefixes the rule name, appends `/by:<initiator>`, and gives a unique key when there is no unit or no initiator. Add a hold state to `bindingState` and a hold timeout to `Config`/`WorkspaceSettings`; `TaskEnded` is where a binding currently resets at once. Binding choice is most recently used, in `bindingFor`. A reservation is always an idle pod (`reservable`).
- 10: an evictable hold is the extra reservation kind. `binding.openedAt` is set for the maximum lifetime, and `Tick` is the place for lifetime drain, grace failures and hold expiry. Resets for a manager restart or a removed workspace are not in the module yet.
- Executor: `applyLocked` in `pool_lending.go` maps each decision to the existing executor code (`startResetLocked`, `closeLeaseLocked`, forced failures, pod queues and wake).
