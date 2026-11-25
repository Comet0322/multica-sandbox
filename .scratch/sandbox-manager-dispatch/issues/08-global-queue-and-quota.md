# 08: Global queue and quota

**What to build:** The manager chooses the next task by priority across all workspaces (ADR 0001).

- **Queue.** It reads every served workspace's pending tasks, keeps only queued ones, and orders them by priority descending, then by creation time ascending.
- **Quota.** It walks the queue and skips any workspace whose quota is used up. Quota counts only capacity occupied by that workspace's running tasks; bindings in hold do not count.
- **Claim.** It claims from the first workspace whose task can be placed, against a reservation. A claim that returns a different task of that workspace is accepted.

**Blocked by:** 07

**Status:** resolved

- [x] A higher-priority task in one workspace is claimed before an older lower-priority task in another
- [x] Within the same priority, the oldest task is claimed first
- [x] A workspace at its quota is skipped and the next eligible workspace is claimed instead
- [x] Bindings in hold do not count against quota
- [x] Tasks no pod can take stay queued upstream, and no claim is sent
- [x] The manager reads one pending list per served workspace per dispatch tick

## Comments

**Verified** (from `server/`, as uid 1000, on integration tip `6002ce002`; there was no newer tip to merge):

- `gofmt -l` (clean) and `go vet` on `./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/` pass.
- `go test -count=1` on those packages: 296 passed. `go test -race -count=1 ./internal/sandboxmanager/...`: 251 passed.
- Module seam: `lending/queue_test.go` (`TestGlobalQueue`) holds table-driven scenarios with a fake clock. They cover:
  - priority across workspaces, and oldest first within a priority;
  - only `queued` tasks being ordered;
  - skipping a workspace at its quota for the next one;
  - quota counting running tasks, not the room left in a binding;
  - no claim when no pod can take a task, because the pod is busy or does not serve the workspace;
  - accepting a different returned task;
  - an empty claim pausing the workspace until its next list;
  - the deferred-retry claim and unknown lists.
- HTTP seam: in `multiws_queue_test.go`, a dispatch tick reads one pending list per (workspace, runtime). The next claim goes to the workspace with the higher-priority task, and no claim reaches the other workspace.

**"One pending list per served workspace per tick".** v0.6.4 has only `GET /api/daemon/runtimes/{id}/tasks/pending` (coordinator decision 3). `readPending` (`pool_lending.go`, called first in `dispatchOnce`) reads each manager runtime of each served workspace once per tick. It merges them into one list per workspace for `PendingChanged`. With one runtime per agent CLI, a workspace costs one read per CLI, and no read is repeated within a tick. The test asserts one read per workspace because the harness registers one runtime.

**Not verified:** "Bindings in hold do not count against quota". Holds arrive with ticket 09. Quota (`occupied`) counts tasks placed in a binding, tasks waiting for a pod and claims in flight. It never counts bindings, so an empty binding in hold would count 0. Add a hold scenario to `TestGlobalQueue` once 09 lands. The compose smoke was not run.

**Module API added** (`lending/queue.go`):

- `PendingChanged(ws, []Pending{ID, Priority, CreatedAt, Status})` replaces the workspace's list and keeps only `StatusQueued` entries. `PendingUnknown(ws)` forgets the list after a failed read. `RemoveWorkspace` forgets it too.
- `RequestClaim` walks `claimOrder`:
  - First the merged queue, ordered by priority descending, then creation time, then ID. For each workspace, it leaves out as many head tasks as that workspace has claims in flight.
  - Then, in rotation after the last workspace claimed from, the workspaces whose list is unknown or that have not been probed since their last list.

  Each candidate must pass `claimable` (served, not excluded, not backing off, within quota) and have a reservable pod among `req.Pods` that serves it. `pickWorkspace` is gone.
- Decision 4: a claim that no queued task accounts for marks the list probed. A workspace with quota and free capacity therefore gets at most one such claim per list, which is one per tick. An empty claim clears the list and marks it probed, so an unclaimable head task is not claimed in a loop before the next tick. A returned task is removed from the list.

**Behaviour notes.**

- Pending lists are read only on the dispatch tick (`WakeInterval` > 0). Without it, or before the first tick, workspaces are claimed from in turn as in ticket 07. Polling pods use the last lists read.
- Quota still counts claims in flight, so two pods cannot overshoot a quota of 1 while their claims are pending.

**Hold and quota, verified with ticket 09.** `TestGlobalQueue` has the scenario "a binding in hold does not count against its workspace's quota". With quota 1, a second claim is refused while the task runs. It is granted once the task ends and its binding is in hold.
