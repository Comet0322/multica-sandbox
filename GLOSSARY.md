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
The value that decides which tasks may share one pod; tasks of different people never share a key.
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

**Quota**:
The most capacity one workspace's running tasks may occupy at once; pods in hold do not count against it.
_Avoid_: Limit, max leases

**Reservation**:
Capacity the manager sets aside before claiming a task upstream, so every claimed task already has a place to run.
_Avoid_: Booking, pre-allocation

**Eviction**:
Ending a binding that is in hold early, because a task with a different affinity key needs the pod.
_Avoid_: Preemption, reclaim
