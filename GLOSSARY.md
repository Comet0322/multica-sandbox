# Managed Sandbox

Runs Multica agent tasks in company-run execution environments, so members get a cloud runtime without a machine of their own.

ADR 0009 changes several terms: pods are created per task through the Kubernetes API instead of started by GitOps and reset. Terms marked _(ADR 0009)_ already have their new meaning. Terms marked _(until ADR 0009 lands)_ describe the GitOps pool that `.scratch/k8s-executor` replaces.

## Language

**Managed Sandbox**:
The product name members see for the cloud runtime the pool provides. Used only in product-facing names.
_Avoid_: Using "sandbox" for a pod or for the pool

**Pool**:
The pods the manager runs tasks on. _(ADR 0009)_ The manager creates them in one namespace, up to its budget. Until ADR 0009 lands, it is the fixed set of pods registered with one sandbox service.
_Avoid_: Sandbox pool, cluster

**Pod**:
One execution environment that runs the stock Multica daemon and agent CLIs. _(ADR 0009)_ It runs one task at a time and is deleted, never reset.
_Avoid_: Sandbox, container, pool pod, worker

**Pod token**:
The credential that identifies one pod to the manager for as long as the pod exists. _(ADR 0009)_ The sandbox service sets it in the pod's environment when it creates the pod. Until then, the pod receives it when it registers.
_Avoid_: Pod IP, boot nonce, pool placeholder

**Manager**:
The program that acts as a daemon toward the Multica server and as the server toward the pods' daemons, and decides which pod runs which task. It reaches pods only through the sandbox service.
_Avoid_: Broker, controller, pool manager

**Core**:
The part of the manager that carries out the policy's decisions and keeps the record of every task it dispatched. It knows tenants, opaque tasks and pod IDs only.
_Avoid_: Broker, dispatcher, scheduler

**Policy**:
The pure part of the core's decisions:
- which tenant claims next;
- where a claimed task goes;
- whether a pod may be created;
- which pod is deleted when the budget is full;
- when holds end.

It knows no Kubernetes and no Multica. It lives in `internal/policy` (formerly `internal/lending`, the "lending policy").
_Avoid_: Lending policy, scheduler, placement engine

**Task source**:
The part of the manager that is the one daemon Multica sees: it follows the bot account's workspaces, registers and heartbeats their runtimes, and claims and fails tasks when the core says so.
_Avoid_: Upstream adapter, Multica client

**Pool side**:
The part of the manager that serves the pods: the sandbox service, and the Multica server it plays toward the daemon in each pod.
_Avoid_: Worker gateway, executor

**Sandbox service**:
The module that owns the pods themselves. Callers use it through Create, Delete, PutFiles, GetFiles and Exec.
- _(ADR 0009)_ It creates pods from the operator's pod template through the Kubernetes API, watches them, and deletes them.
- Until then, it owns the GitOps pods' registration, control channel, resets, liveness and free set.

_Avoid_: Pod manager, provider, runtime, executor

**Pod template**:
_(ADR 0009)_ The Pod YAML the operator provides (`SANDBOX_POD_TEMPLATE`). It sets the image, resources, probes, security context and anything else of the pod. The sandbox service fills in only the pod's name, labels, annotations, restart policy and pod token, and a neutral hostname (`sandbox`) when the template sets none.
_Avoid_: Pod spec (for the file), manifest

**Acquire**:
Taking a ready pod that serves no one from the sandbox service (a successful Create), so the manager may run tasks on it.
_Avoid_: Create (outside the interface), allocate, check out

**Hand back**:
Returning an acquired pod to the sandbox service (Delete). _(ADR 0009)_ The pod is deleted. Until then, the service resets it before anyone can acquire it again.
_Avoid_: Delete (outside the interface), release, free

**Lease**:
The period during which one pod runs one task.
_Avoid_: Claim, assignment, allocation

**Pool runtime**:
_(ADR 0009)_ The runtime kind whose tasks may use an idle pod of their department that served another key.
_Avoid_: Shared runtime, default runtime

**Clean runtime**:
_(ADR 0009)_ The runtime kind whose pods never serve another key. A clean task gets its key's held pod or a pod that has served no one.
_Avoid_: Fresh runtime, isolated runtime

**Key**:
The issue a task belongs to, else its chat session. A pod is held for one key. Tasks with neither have no key and are never held.
_Avoid_: Affinity key, sharing key, binding key

**Hold**:
The time after a task ends during which its pod waits for the next task of the same key, up to the department's hold timeout. _(ADR 0009)_ It is kept as long as possible but never guaranteed: it ends early when the budget is full. Work that a later pod must see is pushed by the agent.
_Avoid_: Grace, keep-alive, warm pod

**Warm pod**:
_(ADR 0009)_ A new, ready pod that has served no one and belongs to no department or runtime kind until its first task.
_Avoid_: Free pod, spare, standby

**Idle pod**:
_(ADR 0009)_ A pool runtime pod whose hold ended. Another key of the same department may use it, until its maximum idle lifetime.
_Avoid_: Free pod, free (for an idle pod)

**Budget**:
_(ADR 0009)_ The most pods the manager may have at once (`SANDBOX_MAX_PODS`), counting warm, starting, running, held and idle pods.
_Avoid_: Capacity, pool size, quota

**Tenant**:
What the policy scopes quota, guarantees and keys to. For Multica it is a workspace; the policy knows nothing else of it.
_Avoid_: Workspace (inside the policy), customer

**Occupancy**:
The pods a workspace takes up. Fair sharing counts it in whole pods.
- _(ADR 0009)_ Its running, held and idle pods.
- Until then, those its bindings hold, running or in hold, and those reserved for its claims.

_Avoid_: Usage, load

**Guarantee**:
The number of pods kept for one workspace: while it occupies fewer, no other workspace may take the rest, even when it has nothing to run.
_Avoid_: Reservation (that is a claim's), minimum, floor

**Weight**:
A workspace's share of the pods beyond the guarantees: the next claim goes to the workspace with the least occupancy beyond its guarantee per unit of weight.
_Avoid_: Priority (that orders tasks within one workspace)

**Quota**:
The most pods one workspace's running tasks may occupy at once; pods in hold do not count against it.
_Avoid_: Limit, max leases

**Eviction**:
Ending a hold early. _(ADR 0009)_ This happens because the budget is full: the pod is deleted, or an idle pod is given to another key. Until then, it happens because a task with a different key needs the pod.
_Avoid_: Preemption, reclaim

### Until ADR 0009 lands

These go away with the GitOps pool.

**Reset**:
Returning a pod to its clean initial state so it can be lent to anyone. Pods created through Kubernetes are deleted instead.
_Avoid_: Cleanup, wipe, recycle

**Binding**:
The period from a pod's first lease until its reset, during which the pod belongs to one affinity key and may serve several leases.
_Avoid_: Session, allocation

**Capacity**:
The amount of each resource a pod offers to running tasks; the first resource is slots, the number of tasks it runs at once.
_Avoid_: Concurrency, size

**Reservation**:
Capacity the manager sets aside before claiming a task upstream, so every claimed task already has a place to run. With ADR 0009, a ready pod exists before each claim instead.
_Avoid_: Booking, pre-allocation
