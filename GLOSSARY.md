# Managed Sandbox

Runs Multica agent tasks in centrally run execution environments, so members get a cloud runtime without a machine of their own.

## Language

**Managed Sandbox**:
The product name members see for the cloud runtime the pool provides. Used only in product-facing names.
_Avoid_: Using "sandbox" for a pod or for the pool

**Pool**:
The pods the manager runs tasks on. The manager creates them in one namespace, up to its budget.
_Avoid_: Sandbox pool, cluster, pool size

**Pod**:
One execution environment that runs the stock Multica daemon and agent CLIs. It runs one task at a time and is deleted, never reset.
_Avoid_: Sandbox, container, pool pod, worker, slot

**Pod token**:
The credential that identifies one pod to the manager for as long as the pod exists. The sandbox service sets it in the pod's environment (`SANDBOX_POD_TOKEN`, values starting `mpt_`) when it creates the pod. Go identifiers spell it `ticket` (`PodTicketEnv`), since scanners take any string named "token" for a hard-coded credential.
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
The module that owns the pods themselves. Callers use it through Create, Delete, PutFiles, GetFiles and Exec. It creates pods from the operator's pod template through the Kubernetes API, watches them, records on each pod what it serves, and deletes them.
_Avoid_: Pod manager, provider, runtime, executor

**Pod template**:
The Pod YAML the operator provides (`SANDBOX_POD_TEMPLATE`). It sets the image, resources, probes, security context and anything else of the pod. The sandbox service fills in only the pod's name, labels, annotations, restart policy and pod token, and a neutral hostname (`sandbox`) when the template sets none.
_Avoid_: Pod spec (for the file), manifest

**Acquire**:
Taking a ready pod that serves no one from the sandbox service (a successful Create), so the manager may run tasks on it.
_Avoid_: Create (outside the interface), allocate, check out

**Hand back**:
Returning an acquired pod to the sandbox service (Delete). The pod is deleted.
_Avoid_: Delete (outside the interface), release, free, reset

**Lease**:
The period during which one pod runs one task.
_Avoid_: Claim, assignment, allocation

**Pool runtime**:
The runtime kind whose tasks may use an idle pod of their department that served another key.
_Avoid_: Shared runtime, default runtime

**Clean runtime**:
The runtime kind whose pods never serve another key. A clean task gets its key's held pod or a pod that has served no one.
_Avoid_: Fresh runtime, isolated runtime

**Key**:
The issue a task belongs to, else its chat session. A pod is held for one key. Tasks with neither have no key and are never held.
_Avoid_: Affinity key, sharing key, binding key

**Hold**:
The time after a task ends during which its pod waits for the next task of the same key, up to the department's hold timeout. It is kept as long as possible but never guaranteed: it ends early when the budget is full. Work that a later pod must see is pushed by the agent.
_Avoid_: Grace, keep-alive, warm pod, binding

**Warm pod**:
A new, ready pod that has served no one and belongs to no department or runtime kind until its first task.
_Avoid_: Free pod, spare, standby

**Idle pod**:
A pool runtime pod whose hold ended. Another key of the same department may use it, until its maximum idle lifetime.
_Avoid_: Free pod, free (for an idle pod)

**Budget**:
The most pods the manager may have at once (`SANDBOX_MAX_PODS`), counting warm, starting, running, held and idle pods.
_Avoid_: Capacity, slots, pool size, quota

**Tenant**:
What the policy scopes quota, guarantees and keys to. For Multica it is a workspace; the policy knows nothing else of it.
_Avoid_: Workspace (inside the policy), customer

**Occupancy**:
The pods a workspace takes up, counted in whole pods by fair sharing: its running, held and idle pods.
_Avoid_: Usage, load

**Guarantee**:
The number of pods kept for one workspace: while it occupies fewer, no other workspace may take the rest, even when it has nothing to run.
_Avoid_: Reservation (a ready pod exists before each claim instead), minimum, floor

**Weight**:
A workspace's share of the pods beyond the guarantees: the next claim goes to the workspace with the least occupancy beyond its guarantee per unit of weight.
_Avoid_: Priority (that orders tasks within one workspace)

**Quota**:
The most pods one workspace's running tasks may occupy at once; pods in hold do not count against it.
_Avoid_: Limit, max leases

**Eviction**:
Ending a hold early because the budget is full: the pod is deleted, or an idle pod is given to another key.
_Avoid_: Preemption, reclaim
