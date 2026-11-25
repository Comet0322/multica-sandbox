Status: resolved

# Sandbox service: pods behind a create / delete / files / exec seam

## Problem Statement

The manager does two jobs in one tangle of code:

- **Talking to Multica and lending pods.** It reads queues, claims tasks, keeps bindings and holds, and plays the Multica server toward each pod's daemon.
- **Running the pods themselves.** It registers them, issues pod tokens, polls their supervisors over the control channel, resets them, and decides when one is lost.

Both live in the same structs and under the same lock. The lending policy tracks whether a pod is resetting or lost as well as its binding. So the execution plane cannot be replaced without rewriting the dispatcher. That covers long-lived pods registered by GitOps today, and something else later.

The operator wants the pod side behind a small interface first, in the same process, so that:

- the dispatcher no longer knows how pods are registered, reset or lost;
- a later execution plane only has to implement that interface;
- the next steps can follow on top of this seam: own protocol types, a separate process, a separate repository.

The pods, the image, every HTTP endpoint the pods call and the deployment manifests stay exactly as they are.

## Solution

A new **sandbox service** module owns everything about pods:

- registration and pod tokens;
- the control channel and resets;
- liveness, losing and pruning pods;
- the set of free pods.

The dispatcher uses it only through a small interface:

| Function | What it does with GitOps-started pods |
| --- | --- |
| **Create** | Takes a free, clean pod and hands it to the caller. This is not real provisioning. |
| **Delete** | Hands a pod back. The service resets it and frees it when the reset completes. |
| **PutFiles / GetFiles** | Not supported by the current supervisor. Returns an error. |
| **Exec** | Not supported by the current supervisor. Returns an error. |

Two pieces of in-process plumbing sit beside the interface:

- Authenticating a pod token to a pod ID, for the daemon protocol on the shared listener.
- Notifying the dispatcher when a pod it holds is interrupted or lost.

The lending policy only sees pods the dispatcher currently holds. Holds, bindings, affinity, the global queue and quota keep their behaviour.

## User Stories

1. As an operator, I want the pods, the image and every endpoint they call to stay the same, so that this refactor needs no redeploy of pods and no manifest change.
2. As an operator, I want one module to own pod registration, pod tokens, the control channel, resets, liveness and pruning, so that replacing the execution plane touches one module.
3. As a developer, I want the dispatcher to reach pods only through Create, Delete, PutFiles, GetFiles and Exec, so that it cannot depend on how pods are run.
4. As a developer, I want the sandbox service to import nothing from the manager, the lending policy or Multica, so that it can later move to its own process or repository unchanged.
5. As a developer, I want Create to return a free pod that has been reset, or a clear "no capacity" error, so that a reservation is made by a successful Create.
6. As a developer, I want Delete to return at once and the pod to become free only after its reset completes, so that a pod is never lent while it still holds an earlier task's files.
7. As a developer, I want PutFiles, GetFiles and Exec to exist and to return a "not supported" error on the current pods, so that a future execution plane can implement them without changing the interface.
8. As an operator, I want a pod the service has never seen reset, and a lost pod reset when it returns, before Create can hand it out, so that the guarantees of the current reset rules hold.
9. As an operator, I want a reset that is not reported done in time to make the pod lost, as now, so that a stuck pod never returns to service.
10. As a developer, I want the service to tell the dispatcher when a pod it holds is interrupted or lost, so that the tasks on it are failed and its binding ends as now.
11. As an operator, I want a held pod that is lost or interrupted to have its running tasks failed, and its placed tasks placed elsewhere, as now, so that work is never stuck on a vanished pod.
12. As an operator, I want the reserve-then-claim rule kept, so that every claimed task still has somewhere to run (ADR 0001).
13. As an operator, I want a pod the dispatcher acquired but never ran a task on to be kept for the next claim instead of handed back, so that an empty claim does not cause a reset.
14. As an operator, I want a pod whose binding ends (hold expiry, maximum lifetime, a unique key, workspace removal) handed back with Delete, so that it is reset as now.
15. As an operator, I want eviction of a hold to keep its current behaviour, so that hold never shrinks usable capacity:
    - the held pod is reserved;
    - the claim is made;
    - when a task of another affinity key comes back, the hold ends, the pod is handed back, and the task waits for the next clean pod.
16. As an operator, I want tasks waiting for a pod after an eviction or a lost pod to be placed before any new claim, so that a claimed task is never starved by later claims.
17. As an operator, I want a pod's daemon polling for work to receive only tasks already placed on it, so that every upstream claim is made against a pod the dispatcher acquired.
18. As an operator, I want `/v1/inventory` to keep its shape, so that existing tooling keeps working:
    - pod state comes from the sandbox service;
    - binding and lease fields come from the dispatcher.
19. As an operator, I want the audit log to keep recording reset start, completion and timeout with the task that caused the reset, so that the trail does not get shorter.
20. As an operator, I want every environment variable to keep its name and meaning, so that no configuration changes.
21. As an operator, I want the eight end-to-end scenarios against Multica v0.6.4 to pass unchanged, so that the refactor is proven not to change what pods and Multica see.

## Implementation Decisions

### Module boundary

- **Location.** The sandbox service lives in its own package, `server/internal/sandboxmanager/sandbox`.
- **Imports.** It imports only the standard library and generic helpers such as uuid. It never imports the manager package, `lending` or `server/pkg/protocol`. A test enforces this.
- **Wiring.** The manager constructs it and mounts its pod-facing handlers on the existing pod listener. Pods keep calling the same paths with the same tokens.

### The interface the dispatcher uses

```go
type Service interface {
    Create(ctx context.Context) (PodID, error)
    Delete(ctx context.Context, id PodID) error
    PutFiles(ctx context.Context, id PodID, dst string, tar io.Reader) error
    GetFiles(ctx context.Context, id PodID, src string) (io.ReadCloser, error)
    Exec(ctx context.Context, id PodID, cmd Command) (Result, error)
}
```

- **Create** returns a pod that is registered, live, clean and not held by anyone. With none, it returns `ErrNoCapacity`. Which free pod it picks is the service's choice. It needs no pending reset to complete first: a reset that is still running means the pod is not free.
- **Delete** starts a reset and returns. The pod is not free until the supervisor reports the reset done. If Delete is called on a pod that is lost or unknown, nothing happens and no error is returned.
- **PutFiles, GetFiles, Exec** return `ErrUnsupported` for every pod. `Command` and `Result` are minimal: argv, environment, working directory; exit code, stdout, stderr. Nothing in this spec calls them.
- The names Create and Delete are kept for future execution planes that really provision. For GitOps pods they mean acquire and return.

### Plumbing beside the interface (same process only)

- **`Authenticate(token) (PodID, bool)`.** The daemon protocol handler identifies the calling pod with it.
  - The manager never stores or compares pod tokens.
  - When the service becomes a separate process, this becomes the service forwarding daemon traffic with the pod's ID in a header.
- **A notification hook** set at construction: `Interrupted(id, why)` and `Lost(id, why)`. It fires only for pods currently held by a caller.
  - **Interrupted** comes from the supervisor's `daemon-exited` and `started` events.
  - **Lost** comes from control-channel silence past the heartbeat timeout, or from a reset timeout.
  - The service calls the hook without holding its own lock.
- **Inventory.** An inventory read lists every pod with its ID, label, state (free, held, resetting, lost), registration time and last seen time.

### What moves into the sandbox service

The following move out of the manager into the service and leave the dispatcher's pod record:

- the pod registry and pod tokens;
- `/pod/register` and `/pod/control`, including supervisor events;
- reset IDs, the dirty flag, reset start and timeout;
- marking pods lost, the stall grace period, lost retention and pruning;
- the reset audit records that need no task.

Liveness changes in one way. Today any pod call updates last-seen, daemon calls included. Afterwards the service counts only control-channel traffic. The supervisor polls every 2 seconds, well inside the heartbeat timeout. Daemon calls no longer count.

### What the dispatcher keeps

- The multica adapter (the Upstream client), workspace sync, the daemon protocol handlers, task routing and the `mat_` passthrough.
- Per held pod: the workspaces its daemon registered for, its leases, and the tasks placed on it.
  - A pod may be lent only to a workspace its daemon registered for, as now.
  - The dispatcher learns these workspaces from the daemon's registrations, as now, whether or not it holds the pod. Create may hand out a pod whose daemon has not yet registered for a given workspace. The dispatcher then uses it for another workspace, or keeps it for later.
- Acquiring pods:
  - When the lending policy has no reservable pod for a claim, the dispatcher calls Create and records the pod with the policy.
  - It keeps acquired pods that never ran a task.
  - It calls Delete whenever the policy decides a reset.
- Claims are made only by the dispatch tick. `claimFor` only hands out tasks already placed on the polling pod.

### Lending policy changes

- **Pod states.** The policy knows only held pods. The resetting and lost states, the `needsReset` flag and `ResetDone` go.
  - `PodRegistered` becomes "a pod was acquired, with the workspaces its daemon serves".
  - A Reset decision means "hand the pod back". The dispatcher carries it out with Delete, and the policy forgets the pod at once.
- **Lost, interrupted and replaced pods.** `PodLost` and `PodInterrupted` keep their task handling: running tasks fail, and placed tasks wait for another pod. The pod is forgotten afterwards. `PodReplaced` is merged into this.
- **Eviction.** Reserving a held pod, claiming, then evicting it for a task of another key stays.
  - Today the evicted-for task waits on the same pod (`podState.next`).
  - Instead it joins the waiting tasks, and the pod is handed back. The next pod the dispatcher acquires takes it.
  - `RequestClaim` already claims nothing while tasks wait, so the claimed task is placed before any new claim.
- **Unchanged:** affinity rules, bindings, hold and its countdown, maximum lifetime and drain grace, the global queue, quota and the probe claim.

### Inventory, audit and configuration

- **Inventory.** `/v1/inventory` merges the service inventory with the dispatcher's bindings and leases. The JSON shape is unchanged.
- **Audit.** The dispatcher records "reset started", naming the lease that caused it. The service records completion and timeout per pod. The dispatcher passes the cause along with Delete only as an audit label.
- **Configuration.**
  - `SANDBOX_POOL_HEARTBEAT_TIMEOUT`, `SANDBOX_POOL_RESET_TIMEOUT` and `SANDBOX_POOL_LOST_RETENTION` configure the service.
  - Every other variable configures the dispatcher.
  - Names, defaults and the overrides file are unchanged.

### Unchanged

- **Pods:** supervisor, entrypoint, image, skip list and reset behaviour.
- **Interfaces and deployment:** the HTTP API toward pods and toward Multica, the k8s and compose manifests, and the admin listener.
- **Decisions:** ADRs 0001–0003. ADR 0003's open registration endpoint stays where it is, now inside the service.

## Testing Decisions

- A good test drives a module through its public seam with a fake clock and asserts outcomes, not internal fields.
- **The sandbox service** gets its own tests at its seam: the HTTP handlers for pods, the interface for callers, and an injected clock. They take over the existing registration, pod-token, control-channel, reset, reset-timeout, lost, stall-grace and prune tests from the manager package. Prior art: `pool_register_test.go`, `pool_reset_test.go`, `pool_liveness_test.go`, `pool_prune_test.go`, `pool_pod_event_test.go`.
- **The lending policy** tests drop the resetting and lost states and cover acquisition, hand-back, and eviction through the waiting list. Prior art: the `lending/*_test.go` scenario tables.
- **The dispatcher** tests run against a fake `sandbox.Service`. They cover:
  - Create on demand and keeping clean pods;
  - Delete on every reset decision;
  - interrupted and lost notifications;
  - a workspace the acquired pod's daemon has not registered.

  Prior art: the harness in `harness_test.go` and the `multiws_*` tests.
- **Wiring tests** keep a few end-to-end checks through the real service and the HTTP handlers, so the shared listener and the token check are covered.
- **Import rule.** A test asserts that the `sandbox` package imports nothing from the manager, `lending` or `server/pkg/protocol`.
- **End to end.**
  - `deploy/sandbox/pool/compose/smoke/scenarios.sh` passes against `ghcr.io/multica-ai/multica-backend:v0.6.4` without edits to the scenarios.
  - Supervisor tests are untouched and pass.

## Out of Scope

- The manager's own copies of the protocol types it imports today, which would drop `server/pkg/protocol`.
- Running the sandbox service as a separate process, forwarding daemon traffic with a pod-ID header, and the NetworkPolicy change that goes with it.
- Moving the code to a separate repository or Go module.
- Implementing PutFiles, GetFiles or Exec, and any change to the supervisor or pod image.
- Changes to upstream polling cost, such as subscribing to the upstream daemon WebSocket. That is handled separately.

## Further Notes

- Work on branch `feat/sandbox-service-split`, which starts from `feat/sandbox-manager-dispatch` at `a1561c708`. Nothing is pushed.
- Keep the constraints of the previous spec:
  - no LLM or git credentials in the manager (ADR 0002);
  - no compatibility shims;
  - never touch `multica-postgres-1` or the compose project `multica-selfhost`;
  - no bare `git stash`.

## Comments

**Resolved** on `feat/sandbox-service-split` (tickets 01–04 resolved; see each ticket's Comments).

**Code review** (Standards and Spec axes, since `a1561c708`).

Fixed in `ffd8a6f8d..3b252f1a8`:

- **Notifications during acquire.** A Lost or Interrupted notification that arrives during an acquire now takes effect at once.
- **Reset audit.** The service writes "reset started" only when Delete really starts a reset.
- **Pod lookup.** The single-pod lookup `Pod(id)` replaces full inventory scans.
- **Rename.** `sandbox.Pool` is now `sandbox.GitOpsService`.
- **Duplication.** The duplicated PodLost helper is now one.
- **Comments.** The stale `SANDBOX_POOL_WAKE_INTERVAL` comment, the roadmap wording in the package comment, and the POOL.md audit line are fixed.

The coordinator also fixed `TestTwoIdlePodsTwoTasksEachGetOneWithoutDuplicates`. Ticket 03 had made it flaky: only the tick claims, and a task may land on either pod. The test now runs both ticks before the pods poll.

**Accepted, not fixed:**

- `boundText` and `AuditRecord` are copied across the package boundary, because the import rule forbids sharing them.
- The manager keys pods by `string` IDs.
- `Create` and `Delete` ignore their `ctx`.
- `PrunedTotal` is in the inventory.
- Delete resets a free pod.
- The service resets interrupted pods itself.

**Verified on the final tip:**

- `gofmt`, `go vet` and `go test -race -count=3 ./internal/sandboxmanager/...` pass: 1077 tests. The full race run of the 5 packages also passes.
- The flaky test passed 50 runs in a row.
- `scenarios.sh` ran against v0.6.4 on `3b252f1a8`, the tip before the test-only fix: 8/8 passed, with no backend errors.

**Not verified:** a real k8s cluster.
