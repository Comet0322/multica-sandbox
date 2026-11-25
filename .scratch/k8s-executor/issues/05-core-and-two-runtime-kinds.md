# 05: Core and two runtime kinds

**What to build:** The core runs the new policy on the Kubernetes sandbox service. The task source registers a pool runtime set and a clean one, as `SANDBOX_RUNTIMES` says.

- **Task source:**
  - Registers, heartbeats and recovers orphans for each kind under its own daemon ID. The clean ID is derived from the pool ID, for example with a `-clean` suffix.
  - Each kind has its own name and device name settings.
  - A task's kind is the kind of the runtime it was claimed for.
- **Core:**
  1. Asks the policy which department and kind to claim for.
  2. Makes sure a ready pod exists (warm or new) before claiming.
  3. Claims, then places the task where the policy says. The pod readied for the claim stays warm if the task goes elsewhere.
  4. Deletes the pods the policy names.
- **Pool side:** a pod's daemon claims and gets the task placed on it. This is unchanged.
- **Turning a kind off:** removing a kind from `SANDBOX_RUNTIMES` stops heartbeating its runtimes, so they show offline in Multica, as a removed workspace's do.

**Blocked by:** 02, 04

**Status:** resolved

- [x] Tests with fakes of the task source, sandbox service and pool side cover a pool and a clean task end to end, a handoff landing on the held pod, a held pod deleted for the budget, and a claim that returns nothing
- [x] `SANDBOX_RUNTIMES=pool`, `clean` and `pool,clean` each register exactly the expected runtimes, and the default is `pool`
- [x] `gofmt`, `go vet` and `go test -race ./...` pass

## Comments

Branch `ticket/04-05-policy`, commits `f77b78c` and `1307e5d` (docs).

**Decisions:**
- **Daemon IDs:** the pool kind keeps the manager's daemon_id (and its legacy id); the clean kind registers and claims under it followed by `-clean` (`tasksource.DaemonIDSuffix`). Orphan recovery treats each kind's own daemon_id as its own.
- **Names:** `SANDBOX_MANAGER_CLEAN_NAME` (default `SANDBOX_MANAGER_NAME` followed by ` Clean`, since names may not hold parentheses) and `SANDBOX_MANAGER_CLEAN_DEVICE_NAME` (default the clean name). A workspace's own runtime name applies to the pool kind only.
- **A pod's daemon hosts the runtimes of every kind:** the answer to its registration lists both kinds' runtimes. The stock daemon fails a claimed task whose `runtime_id` it does not host (`handleTask`, "runtime went offline before the run started"), and a pod may be given a task of either kind.
- `SANDBOX_RUNTIMES` is read case-insensitively, comma separated; an unknown or repeated kind is refused.
- The default hold timeout is now 10 minutes, as ADR 0009 says (it was 15).
- `deploy/k8s/dev/manager.sh` runs `pool,clean` and drops the placement and capacity settings.

**Not done here:** an end-to-end run against the kind namespace with both kinds (issue 08), and the dispatch page showing kinds and phases (issue 07).
