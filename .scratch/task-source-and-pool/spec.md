Status: ready-for-human

# The manager between a task source and a pool, with weighted fair sharing

Design conclusions from a design discussion (2026-11-14). Implemented on `refactor/multica-adapter` (see ADR 0008): the core, the task source and the pool side, the task source's own heartbeat, and weighted fair sharing with guarantees. Pod sharing (Open 1) stays as it was: bindings, holds and slots. Decisions marked **Open** still need the operator. This builds on ADR 0007, whose "Considered options" named the task source / pool split as the next step.

## Problem Statement

What the manager knows of Multica is gathered in `internal/multica`, but the manager still runs through it: workspaces, runtimes and their drain sit in the manager's state, and the daemon protocol asks the manager routing questions (`taskRoute`, `reposRoute`, `mayReadRepos`, `runtimeServed`). The same facts are kept in several places and repaired by reconciliation:
- `Manager.held` and the sandbox service;
- `daemonWorkspaces` and the policy's pod tenants;
- `claimed`, `pod.queue`, `leases`, `history` and the policy's tasks.

Workspaces are not treated fairly. Claims follow one global queue ordered by priority and then age, so:
- one workspace's burst takes every free pod up to its quota, and quota defaults to none;
- priorities set inside one workspace outrank another's;
- holds do not count;
- a pod is counted in slots, although affinity makes the whole pod one workspace's.

## Components

The manager core faces exactly two sides.

**Task source** (Multica-facing; plays one daemon toward Multica):
- Lists the bot account's workspaces and turns each into a tenant, with its overrides settings (quota, affinity rule, hold timeout, maximum lifetime, and new: guarantee and weight).
- Registers and keeps the runtimes in each workspace: visibility, orphan recovery, re-registration after a runtime is deleted.
- Heartbeats upstream for as long as it runs. The runtime is always online. With no pod, nothing is claimed, so tasks stay queued in Multica (ADR 0001).
- Reads pending lists for the core, claims when the core says so, and fails tasks when told, retrying failed reports.
- Does not know pods, leases or usage.

**Manager core** (decisions and bookkeeping; no HTTP, no Multica or daemon JSON):
- Feeds facts to the lending policy and carries out its decisions:
  - tells the pool side to acquire or hand back a pod, place a task on a pod, or withdraw a task;
  - tells the task source to claim or fail.
- Keeps the single dispatch record of every task, from claim to end: the claimed payload, its pod, start, end and reason. This replaces `claimed`, `pod.queue`, `leases` and `history`. The policy keeps only what it decides with.
- Serves the admin API and the audit log.

**Lending policy** (pure, inside the core): quota, affinity, bindings, holds, eviction, reservations, and the fair share below.

**Pool side** (pod-facing; the daemon protocol, with the sandbox service inside it):
- Offers the core one resource, a pod: acquire a free reset pod or report none, hand a pod back, and tell the core when a pod appears, is lost or is interrupted.
- Plays the Multica server toward the stock daemon: workspace list, register, heartbeat, claim, wake socket.
- Holds pod tokens, and which tenants each pod's daemon registered for, including free pods.
- Has no dispatch record of its own. When a daemon claims, it asks the core what the pod should run. When a daemon reports on a task or reads repos, it asks the core whether that is the pod's task or tenant, then forwards with the account's PAT.
- Tells the core when a terminal report was accepted. Relays agents' `mat_` calls unchanged.
- The sandbox service stays generic and unchanged.

**Daemon and agents**: stock, unchanged.

## Interactions

- **Task source and core**: tenants and their settings, pending lists, claim, fail, drain.
- **Pool side and core**: pods, placing and withdrawing tasks, task ends, and which tenants a pod can serve.
- **Task source and pool side, directly**: only what does not touch lending.
  - The model list request goes straight from the task source to the pool side.
  - Wiring is done in `cmd`, so neither package imports the other.
- A task is delivered by pull, because the stock daemon only pulls:
  1. The core decides on a task and a pod.
  2. The pool side wakes the pod.
  3. The daemon claims.
  4. The pool side hands over the payload the core keeps for that pod.

## Fair sharing between workspaces

Each workspace has three settings, in pods:

- **Guarantee** (new; default 0): reserved for the workspace.
  - Never lent to others while idle.
  - The workspace's own running and holding pods count toward it.
  - The guarantees must not sum to more than the live pods. When pods are lost and they do, they are scaled down proportionally, and the admin API shows the shortfall.
- **Weight** (new; default 1): shares the pods beyond the guarantees.
- **Quota** (existing): the hard maximum.

How the next claim is chosen:
- Among workspaces with queued tasks and room under their quota, the next claim goes to the one with the least `(pods occupied beyond its guarantee) / weight`.
- Ties go to the longest-waiting task.
- Priority and age order tasks only inside one workspace.
- Occupancy is counted in whole pods, holds included. A pod the core acquired that has no binding counts as free.
- The share is instantaneous, not historical. It is work-conserving: a lone busy workspace may use every pod up to its quota.
- Running tasks are never preempted. Pods come back only when they are handed back. A workspace over its share loses its holds first to eviction.
- Rebalancing is therefore bounded by maximum lifetime plus drain grace: 2h10m by default.

**Live pods**, the base of the guarantees: free, held and resetting pods. Lost pods are left out. The pool side reports this; the core cannot create or delete pods, which GitOps does.

New settings are optional fields in the overrides file. An old overrides file behaves as an equal share.

## Facts established

- Multica's pending API (`ListPendingTasksByRuntime` → `taskToResponse`) already returns `chat_session_id` and `attribution.originator.id`. These are the fields affinity keys are read from, so a task's affinity key is known before it is claimed. The manager currently drops them (`lending.Pending`). No database access is needed (ADR 0001 stands).
- A claim cannot pick a task. The server picks by priority and age, subject to per-(agent, issue / chat session) serialization and wakeup checks. Within one workspace the pick can be predicted, not forced. The choice between workspaces is the manager's alone, since each workspace has its own runtimes.
- A pod goes back to the sandbox service when:
  - its binding's last task ends with no hold (hold timeout 0, or a key unique to the task);
  - its hold expires;
  - its binding passes its maximum lifetime (at once in hold; after its tasks end; tasks failed after the drain grace);
  - it is evicted;
  - its workspace is removed.

## Open

1. **Pod sharing.** Keep bindings, holds and slots, using the pending list's affinity keys to reserve better? Or one task per pod, with files, sessions and caches kept per affinity key outside the pod and restored with `PutFiles` / `GetFiles` before the daemon claims? The latter must restore within Multica's ~90 s start window and needs external storage. Fair sharing works either way.
   **Resolved by ADR 0009** (2026-11-16):
   - The manager creates pods through the Kubernetes API, and a pod runs one task at a time.
   - A pod is held for its issue or chat session as long as possible, but not guaranteed.
   - The manager keeps nothing outside the pod: agents push what a later pod must see through the company's git MCP.
   - A pool and a clean runtime kind replace the affinity rules.

   See `.scratch/k8s-executor/spec.md`.
2. **Drain owner.** The proposal is that the task source reports a vanished workspace and the core drains it, then tells the task source to close it.
3. **Withdrawal.** The proposal is that when the core ends a lease itself (lifetime, eviction, drain), it tells the pool side, which refuses that task's later reports. Stopping the run is a reset.
4. **Registration path.** The proposal is that the pool side gets the registration answer from the task source directly, and tells the core only that the pod can serve the tenant.
5. **Smaller proposals not yet confirmed:**
   - pod tokens live only in the pool side;
   - both adapters share the Multica HTTP client;
   - the admin `leased` field keeps its meaning, with an added field for occupancy;
   - first collapse the duplicate records inside the manager.
6. **Naming.** "Worker gateway" uses a term GLOSSARY avoids. The pool side and the core need glossary names before ADR 0008.
