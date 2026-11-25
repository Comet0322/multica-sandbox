# 10: Max lifetime, eviction and forced resets

**What to build:** No binding lives forever, and hold never shrinks usable capacity.

- **Maximum lifetime.** A binding past its maximum lifetime takes no new tasks and is reset once its tasks end. Tasks still running after the drain grace period are failed.
- **Eviction.** When a task with a different affinity key needs a pod and none is free, the binding in hold that has been idle longest is evicted. A binding with running tasks is never evicted.
- **Forced resets.** These happen when a pod is lost, when the manager restarts (every pod is reset and its running tasks failed upstream), and when a workspace is removed after the drain timeout.

**Blocked by:** 09

**Status:** resolved

- [x] A binding past its maximum lifetime gets no new placement and is reset when its last task ends
- [x] A task still running after the drain grace period is failed upstream with a clear reason
- [x] With the pool full of holds, a new task with another key evicts the hold idle longest
- [x] A binding with a running task is never evicted, even when that leaves the task unclaimed
- [x] A lost pod, a manager restart and a removed workspace each produce the documented resets and failures
- [x] Every eviction and forced reset is in the audit log with its reason

## Comments

**Verified** (from `server/`, as uid 1000):

- `gofmt -l` (clean) and `go vet` on `./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/` pass.
- `go test -count=1` on those packages: 359 passed, none skipped. `go test -race -count=1 ./internal/sandboxmanager/...`: 310 passed.
- Module seam, table-driven with a fake clock:
  - `lending/lifetime_test.go` (`TestMaximumLifetime`, `TestRemovedWorkspaceResetsItsPods`): no placement past the lifetime and a reset at the last end; a hold reset when it reaches the lifetime; failure, lease end and reset after the drain grace; no lifetime for unique keys or a lifetime of 0; a removed workspace's hold reset at once and its running binding at its last end.
  - `lending/eviction_test.go` (`TestEviction`): the hold idle longest is evicted and the task placed after `ResetDone`. A reserved hold takes its own key's task (renewal). A task matching another binding goes there and the hold stays. An empty claim leaves the hold. An idle pod is reserved first. No hold is reserved while any pod serving the workspace is idle, or when the asking pod is not the hold idle longest. A running binding is never reserved. A task whose evicted pod is lost evicts the next hold.
- HTTP seam: the three `t.Skip("ticket 10: …")` tests are revived (lifetime drain, grace failure) or rewritten (`TestHeldPodIsEvictedForATaskOfAnotherKeyWhenNoPodIsFree`, with the eviction audit). New tests:
  - `TestHeldPodClaimsItsKeysNextTaskWhenNoPodIsIdle`: the ticket 09 gap.
  - `TestRunningPodIsNeverEvicted`: the task stays queued upstream.
  - `TestLostHeldPodIsAuditedAndResetWhenItReturns`.
  - `TestReloadResetsTheRemovedWorkspacesPods`: a hold reset at removal, a running binding reset at the drain timeout, both audited `workspace removed`.
- Supervisor seam: `TestUnknownPodTimeoutEndsTheSupervisor/401_ends_it_at_once`.
- Compose smoke (`smoke.sh`, project `t10smoke`, backend `multica-backend:pooldev`, own Postgres, torn down with `down -v`): PASS. It needed `SANDBOX_HOLD_TIMEOUT: "0"` in `smoke.yaml`: since ticket 09's 15m default the first task's binding held the pod and the smoke's "reset between tasks" step timed out.
- Manager restart, by hand on the smoke stack (`KEEP=1`): `docker restart` of the manager. The supervisor logged "refuses this pod's token … exiting" at once, the container restarted (restart count 1), and the pod registered as a new pod about 2 s after the manager. It was reset ("pod registered; it is reset before it is first lent") before it was lent.

**Not verified:** the k8s manifests on a cluster; a v0.6.4 backend (the smoke used `pooldev`, as ticket 04 did). The manager restart with a task running was not run by hand. The upstream failure of that task comes from orphan recovery at the first registration (`pool_recover_orphans_test.go`).

**What changed.**

- Lending policy (`lending/policy.go`):
  - `Config.MaxLifetime` / `DrainGrace` (0 = none), fed from `SANDBOX_POD_MAX_LIFETIME` / `_DRAIN_GRACE` (defaults 2h / 10m, now applied). The lifetime counts from the binding's opening; renewal does not extend it.
  - A binding past it is skipped by `bindingFor`. `TaskEnded` resets it instead of holding it (`maximum lifetime reached`), and `Tick` resets it in hold.
  - Past lifetime + grace, `Tick` fails its started tasks ("the sandbox pod reached its maximum lifetime and the task ran past the drain grace period"), ends their leases and resets the pod (`maximum lifetime and drain grace passed`). Unstarted tasks there wait for another pod.
  - A binding whose key is unique (`task:`) has no lifetime: it ends with its task, and `SANDBOX_POOL_LEASE_MAX` bounds a stuck task. This keeps `TestTaskPlacementIgnoresSharedLifetimes`.
- Reservation (`reservePod`): the first idle pod among the request's pods; else, only when no pod serving the workspace is idle anywhere, the pod whose hold started longest ago (`heldSince`), provided it is among the request's pods.
  - In `ClaimReturned`, a task matching a binding with room goes there (most recently used first) and the reservation is released.
  - Otherwise, a reserved hold is evicted: `Reset{evicted for task <id> of another affinity key}`. The task is kept on the pod (`podState.next`, counted in quota and `Busy`) and opens its binding at `ResetDone`. If that pod is lost, the task joins the waiting list.
  - `placeWaiting` and `placeOrphan` may evict too.
  - Holds whose pod is reserved are left to the claim's outcome (`Tick` skips them).
- `RemoveWorkspace` now returns decisions. It resets the workspace's holds (`workspace removed`), and `TaskEnded` resets, instead of holding, a binding whose workspace is no longer served. The drain timeout's lease ends therefore reset the pod too.
- Executor: `claimFor` lets a pod in hold claim, as well as an idle one. A heartbeat-timed-out pod is audited (`action: reset`, `deny`, `pod lost: heartbeat timed out`).
- Manager restart (coordinator decision 12): the supervisor treats a 401 on its control poll as "the manager refuses my token" and exits at once, so the container restarts and registers again. 404 keeps the 5-minute `UnknownTimeout`. The rest of the forced reset already existed: the registration reset (ticket 04) and orphan recovery, which fails upstream what was running.
- `deploy/sandbox/POOL.md` documents the lifetime, eviction, removal and restart behaviour.

**For ticket 11:** `WorkspaceSettings` has no `MaxLifetime` override yet; the lifetime is pool-wide (`p.cfg.MaxLifetime` in `pastLifetime`).
