# 02: Sandbox service module

**What to build:** A standalone `server/internal/sandboxmanager/sandbox` package that does everything the manager does today for pods themselves. It is fully tested at its own seam. The manager does not use it yet; ticket 03 switches over.

- **Pod-facing handlers.** `/pod/register` and `/pod/control` (GET poll, POST report and events), with the same request and response bodies, status codes and pod-token format (`mpt_`) as now. The supervisor and entrypoint must not notice any difference.
- **The caller interface** (see the spec):
  - `Create` returns a registered, live, clean pod that nobody holds, or `ErrNoCapacity`.
  - `Delete` starts a reset and returns. The pod is free again only after the supervisor reports the reset done. Delete on a lost or unknown pod does nothing.
  - `PutFiles`, `GetFiles` and `Exec` return `ErrUnsupported`.
  - Delete takes an audit label for the reason, so the dispatcher can name its cause.
- **Plumbing:**
  - `Authenticate(token) (PodID, bool)`.
  - An inventory read: ID, label, state free/held/resetting/lost, registration time, last seen.
  - A notification hook set at construction: `Interrupted(id, why)` on `daemon-exited` and `started` for held pods, and `Lost(id, why)` for held pods. The hook is called without the service's lock held.
- **Rules carried over from the manager unchanged:**
  - A pod the service has never seen is reset before Create can return it, and so is a lost pod that comes back.
  - A reset not reported done within the reset timeout makes the pod lost.
  - Liveness counts control-channel traffic only. A pod silent past the heartbeat timeout is lost.
  - The stall grace period: a manager stall longer than the heartbeat timeout gives every pod one grace period.
  - Lost pods are pruned after the retention period, and their tokens are refused afterwards.
  - A 401 is returned for an unknown token.
- **Audit.** Reset completion and timeout records go through an audit writer the caller injects, in the existing `AuditRecord` shape. Define a minimal writer interface in the package, so it does not import the manager.
- **Imports.** The package imports nothing from the manager, `lending` or `server/pkg/protocol`.

**Blocked by:** 01

**Status:** resolved

- [x] The supervisor's integration tests (`internal/podsupervisor`) pass against the new handlers when they point at a test server of the sandbox service
- [x] Create never returns a pod that is held, resetting, lost or not yet reset since the service first saw it; with none left it returns `ErrNoCapacity`
- [x] After Delete, Create does not return the pod until its reset is reported done; a reset timeout makes it lost; Delete of a lost or unknown pod is a no-op
- [x] `daemon-exited` and `started` on a held pod fire `Interrupted` once; on a free pod they fire nothing
- [x] Control-channel silence past the heartbeat timeout fires `Lost` for a held pod and makes a free pod unavailable; the stall grace period and pruning behave as today
- [x] `PutFiles`, `GetFiles` and `Exec` return `ErrUnsupported`
- [x] A test fails if the package imports the manager, `lending` or `server/pkg/protocol`
- [x] `gofmt`, `go vet` and `go test -race` pass on the package

## Comments

**Verified** (from `server/`, as uid 1000; implementer run, then re-run by the coordinator on integration tip `b36bf60a8`):

- `gofmt -l internal/sandboxmanager/sandbox` clean; `go vet ./internal/sandboxmanager/...` ok.
- `go test -race -count=1 ./internal/sandboxmanager/sandbox/` 36 passed. The implementer also ran `-race` on `./internal/podsupervisor/ ./internal/sandboxmanager/ ./internal/sandboxmanager/lending/`, all ok, with no supervisor test skipped.
- Supervisor checkbox: `supervisor_test.go` `TestTheRealSupervisorRunsAgainstTheService` builds `cmd/pod-supervisor` and runs it against the service. It covers the first reset, `started` on a free pod, a Delete reset removing a planted file, and a killed daemon firing `Interrupted`. The `internal/podsupervisor` tests are untouched.
- Import rule: `imports_test.go` (`go list -deps`) fails on any `github.com/multica-ai/` package other than `sandbox` itself.
- Each test was seen failing once (the held-check and stall grace were broken on purpose, then restored).

**Decisions ticket 03 must follow:**

- **API.**
  - The type is `sandbox.GitOpsService`, built with `sandbox.NewGitOps(Config)`. It satisfies `Service`.
  - Other exports: `Authenticate`, `Inventory()` (pods plus `PrunedTotal`), `Handler()` (serves only `RegisterPath` and `ControlPath`), and `Run(ctx)`, which checks liveness, timeouts and pruning every second.
  - The states are `free`, `held`, `resetting` and `lost`.
- **Delete** is `Delete(ctx, id, AuditLabel{Reason, TaskID, WorkspaceID, Initiator})`, not the spec's two-argument form. It resets held and free pods, and does nothing for resetting, lost or unknown ones.
- **Audit.** The dispatcher must record "reset started" when it calls Delete. The service records "reset started" only for resets it starts itself: on registration, when a lost pod returns, and on interrupt. It also records completion and timeout, both with the caller's label, and loss by heartbeat.
- **Interrupt.** On an interrupt, the service releases the hold, starts the reset itself and then fires `Interrupted`. The dispatcher must not expect to call Delete; if it does, the call is a no-op.
- **Lost pods returning.** Any poll or report from a lost pod starts a new reset. A late report from a timed-out reset never frees the pod. An event from a lost pod now brings it back through a reset, where before it left the pod lost.
- **Timeout scope.** Reset timeouts affect only pods nobody holds, so they never fire `Lost`. Hook calls run after the service's lock is released.
- **Config.** `Config` takes `Now`, plus a separate `Monotonic` clock used only for stall detection. The three timeouts keep the manager's defaults, and negative values are rejected.
