# Managed Sandbox

Runs Multica agent tasks in a fixed set of company-run execution environments, so members get a cloud runtime without a machine of their own.

## Language

**Managed Sandbox**:
The product name members see for the cloud runtime the pool provides. Used only in product-facing names.
_Avoid_: Using "sandbox" for a pod or for the pool

**Pool**:
The fixed set of pods registered with one sandbox service, which its manager lends out.
_Avoid_: Sandbox pool, cluster

**Pod**:
One lendable execution environment that runs the stock Multica daemon and agent CLIs.
_Avoid_: Sandbox, container, pool pod, worker

**Pod token**:
The credential the sandbox service issues to a pod when it registers. It identifies that pod for as long as the pod exists.
_Avoid_: Pod IP, boot nonce, pool placeholder

**Manager**:
The program that acts as a daemon toward the Multica server and as the server toward the pods' daemons, and decides which pod runs which task. It reaches pods only through the sandbox service.
_Avoid_: Broker, controller, pool manager

**Core**:
The part of the manager that decides which pod runs which task and keeps the record of every task it dispatched. It knows tenants, opaque tasks and pod IDs only.
_Avoid_: Broker, dispatcher, scheduler

**Task source**:
The part of the manager that is the one daemon Multica sees: it follows the bot account's workspaces, registers and heartbeats their runtimes, and claims and fails tasks when the core says so.
_Avoid_: Upstream adapter, Multica client

**Pool side**:
The part of the manager that lends the core pods: the sandbox service, and the Multica server it plays toward the daemon in each pod.
_Avoid_: Worker gateway, executor

**Sandbox service**:
The module that owns the pods themselves: their registration and pod tokens, the control channel, resets, liveness and the set of free pods. Callers use it through Create, Delete, PutFiles, GetFiles and Exec.
_Avoid_: Pod manager, provider, runtime

**Acquire**:
Taking a free pod that has been reset from the sandbox service (a successful Create), so the manager may lend it.
_Avoid_: Create (outside the interface), allocate, check out

**Hand back**:
Returning an acquired pod to the sandbox service (Delete), which resets it before anyone can acquire it again.
_Avoid_: Delete (outside the interface), release, free

**Lease**:
The period during which one pod is lent to one task.
_Avoid_: Claim, assignment, allocation

**Reset**:
Returning a pod to its clean initial state so it can be lent to anyone.
_Avoid_: Cleanup, wipe, recycle

**Affinity key**:
The value that decides which tasks may share one pod; tasks of different people never share a key, except under the workspace rule, which a workspace opts into.
_Avoid_: Sharing key, placement unit, binding key

**Binding**:
The period from a pod's first lease until its reset, during which the pod belongs to one affinity key and may serve several leases.
_Avoid_: Session, allocation

**Hold**:
The part of a binding when no task is running and the pod waits for another task with the same affinity key.
_Avoid_: Grace, keep-alive, warm pod

**Capacity**:
The amount of each resource a pod offers to running tasks; the first resource is slots, the number of tasks it runs at once.
_Avoid_: Concurrency, size

**Tenant**:
What the lending policy scopes quota and affinity keys to. For Multica it is a workspace; the policy knows nothing else of it.
_Avoid_: Workspace (inside the lending policy), customer

**Occupancy**:
The pods a workspace takes up: those its bindings hold, running or in hold, and those reserved for its claims. Fair sharing counts it in whole pods.
_Avoid_: Usage, load

**Guarantee**:
The number of pods kept for one workspace: while it occupies fewer, no other workspace may take the rest, even when it has nothing to run.
_Avoid_: Reservation (that is a claim's), minimum, floor

**Weight**:
A workspace's share of the pods beyond the guarantees: the next claim goes to the workspace with the least occupancy beyond its guarantee per unit of weight.
_Avoid_: Priority (that orders tasks within one workspace)

**Quota**:
The most capacity one workspace's running tasks may occupy at once; pods in hold do not count against it.
_Avoid_: Limit, max leases

**Reservation**:
Capacity the manager sets aside before claiming a task upstream, so every claimed task already has a place to run.
_Avoid_: Booking, pre-allocation

**Eviction**:
Ending a binding that is in hold early, because a task with a different affinity key needs the pod.
_Avoid_: Preemption, reclaim
