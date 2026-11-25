# 03: Dispatcher lends pods through the sandbox service

**What to build:** The manager runs on the sandbox service from ticket 02, and its own pod-running code is gone. The lending policy only knows the pods the dispatcher holds.

- **Wiring.**
  - The manager constructs the service and mounts its `/pod/register` and `/pod/control` handlers on the pod listener.
  - The daemon protocol identifies callers with `Authenticate`.
  - The manager no longer stores pod tokens, reset IDs, the dirty flag, reset timers, last-seen or lost state. The registry, `pool_reset.go`'s control handling, `markLostLocked`'s pod checks and pruning are deleted from the manager package.
- **Acquiring and handing back.**
  - When the policy has no reservable pod for a claim, the dispatch tick calls `Create`, then records the pod with the policy along with the workspaces its daemon registered for. `ErrNoCapacity` means there is no reservation this round.
  - Pods that never ran a task are kept, not handed back.
  - Every Reset decision is carried out with `Delete`, labelled with its cause for the audit.
  - `Interrupted` and `Lost` notifications are fed to the policy as `PodInterrupted` and `PodLost`.
- **Claims only on the tick.** `claimFor` hands a polling pod only the tasks already placed on it. It no longer claims upstream.
- **Lending policy.**
  - Remove the resetting and lost pod states, the `needsReset` flag, `ResetDone` and `podState.next`. Merge `PodReplaced` into the lost/interrupted handling.
  - A Reset decision means the pod is handed back and forgotten.
  - Eviction keeps reserving a held pod and claiming. A task of another key then ends the hold, the pod is handed back, and the task joins the waiting tasks. The next acquired pod takes it before any new claim is made.
- **Inventory and audit.**
  - `/v1/inventory` merges the service inventory with bindings and leases, keeping the JSON shape. Map free → `idle`, held → `idle` or `leased`, as today.
  - The dispatcher writes "reset started" naming the lease. The service writes completion and timeout.
- **Configuration.** The heartbeat, reset-timeout and lost-retention settings go to the service. Every variable keeps its name and default.
- **Tests.** Move the pod-side tests to the service package where ticket 02 does not already cover them. The manager tests run against a fake `sandbox.Service`, plus a few wiring tests through the real one.

**Blocked by:** 02

**Status:** resolved

- [x] The manager package no longer contains pod-token, reset-ID, reset-timeout, last-seen or prune logic; the `lending` package has no resetting or lost pod state and no `ResetDone`
- [x] A claim is sent upstream only after a successful Create or against a pod the dispatcher already holds; with `ErrNoCapacity` and no reservable held pod, nothing is claimed and the task stays queued upstream
- [x] An empty claim does not hand the acquired pod back
- [x] Hold expiry, maximum lifetime, a unique key, workspace removal and eviction each end in exactly one Delete for the pod
- [x] An evicted-for task runs on the next pod acquired, and no new claim is made while it waits
- [x] `Interrupted` and `Lost` for a held pod fail its started tasks upstream and re-place its unstarted ones, as today
- [x] A polling pod with nothing placed on it causes no upstream claim
- [x] A pod whose daemon has not registered for a workspace is never lent to that workspace
- [x] `/v1/inventory` output for the existing inventory tests is unchanged
- [x] `gofmt`, `go vet`, `go test` and `go test -race` pass on `./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/`

## Comments

**Verified** (from `server/`, as uid 1000, with no tests skipped). The coordinator re-ran everything on integration tip `ad092a29c`:

- `gofmt -l` reports nothing on the four packages, and `go vet` passes.
- `go test -race -count=1 ./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/`: 407 passed in 5 packages.
- `registry.go` and `pool_reset.go` are gone. `lending` has no `ResetDone`, `needsReset`, `PodReplaced`, resetting or lost state.
- Tests for each checkbox, all in `dispatcher_test.go` against a fake service:
  - `TestAClaimIsMadeOnlyAgainstAnAcquiredPod`
  - `TestAnEmptyClaimKeepsTheAcquiredPod`
  - `TestEveryBindingEndHandsThePodBackOnce`
  - `TestAnEvictedForTaskRunsOnTheNextPodAcquiredBeforeAnyNewClaim`
  - `TestAnInterruptedOrLostPodFailsItsStartedTasksAndReplacesTheOthers`
  - `TestAPollWithNothingPlacedCausesNoClaim`
  - `TestAPodIsNotLentToAWorkspaceItsDaemonDidNotRegisterFor`
- The existing harness tests and `pool_inventory_test.go` run on the real service.
- Each test was seen failing once, with the code broken on purpose and then restored.

**Not verified:** `scenarios.sh` (ticket 04).

**Decisions:**

- **Lending API.**
  - The `Acquire` decision asks for a pod.
  - `PodAcquired(pod, workspaces...)` adds a pod; `PodRegistered` adds a workspace to a held pod.
  - `ClaimRequest.NoCapacity` means the service had nothing to give. Only then is a hold reserved or ended for a waiting task, and a waiting task ends at most one hold.
  - The `Reset` decision means the pod is handed back and forgotten.
- **Calls into the service.** The manager never calls the service while holding its own lock. Hand-backs are queued and sent in `settle()`.
- **Catching missed notifications.** Each tick compares the pods the manager holds with the service inventory, and treats any the service no longer lists as held as lost.
- **Daemon workspaces.** They are kept per pod, held or not, and dropped when the service no longer lists the pod.

**Behaviour changes:**

- **`SANDBOX_POOL_WAKE_INTERVAL=0`.** It no longer turns dispatch off. The tick runs every second (`DefaultDispatchInterval`) and only the wake socket is disabled.
- **Daemon polls.** A poll never claims upstream:
  - It never answers 502 when upstream fails (`TestFailingUpstreamAloneStillAnswersBadGateway` was removed).
  - A heartbeat from a lost pod is no longer answered 404. The pod returns through its control channel instead.
- **Evicted-for tasks.** They now wait under `SANDBOX_PLACEMENT_WAIT` (30s by default) and fail if no clean pod appears in time. Before, they waited on the same pod with no limit.
- **Orphaned tasks.** A task orphaned by a lost reservation now waits for a pod. Before, it was failed at once.
- **Ordering.** Claims follow the global queue on every tick. The two rotation tests now use interleaved creation times.
- **One-tick acquisition.** One tick may acquire every free pod when none serves the wanted workspace.
