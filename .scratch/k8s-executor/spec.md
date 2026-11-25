Status: ready-for-human

# One task per pod, created through the Kubernetes API

Design conclusions from a design discussion (2026-11-15 to 2026-11-16), recorded in ADR 0009. Nothing is implemented yet.

## Problem Statement

The company runs stock Multica v0.6.4 for 16 departments, about 1,400 people. Each department's squad runs on one runtime, which is one pod.

- **Out of memory.** One daemon runs every task of a department with no limit per task. One task that runs out of memory takes the daemon and every other task down with it. The only knob is the daemon's concurrency, which is a guess.
- **Handoff.** A second runtime does not help. A squad hands an issue from one agent to the next, and the next agent checks out the previous agent's branch from the machine's shared bare repo cache (`~/multica_workspaces/.repos`, one worktree per task). The branch is not pushed, so another machine does not have it.
- **Clean pods.** Some work needs a pod no other work has touched.

The manager as built (ADR 0001 to 0008) answers these on GitOps pods that it can only reuse and reset, shared by several tasks, with handoff state kept on the pod. That premise makes a pod's free capacity depend on which task comes next, while a claim cannot choose its task. Bindings, holds, eviction, lifetimes, reservation corrections, slots and the supervisor's reset all follow from it.

## Solution

The manager creates and deletes pods itself through the Kubernetes API, and a pod runs one task at a time. Kubernetes packs pods onto nodes by their requests and limits each pod's memory, so one task running out of memory ends only that task.

```
Multica ── task source ── core ── sandbox service (Kubernetes) ── pods
   └────────── pool side (daemon protocol) ──────────────────────┘
```

### Components

- **Task source** (unchanged in role).
  - Registers each workspace's runtimes once for each kind in `SANDBOX_RUNTIMES`, each kind under its own daemon ID.
  - Heartbeats them, reads pending lists, and claims and fails tasks.
- **Core.**
  - Keeps the dispatch record of every task.
  - Carries out the policy's decisions: create or delete a pod, claim, place, fail.
- **Policy** (`internal/policy`, formerly `internal/lending`). Pure: no Kubernetes and no Multica. It decides:
  - which department claims next;
  - where a claimed task goes;
  - whether a pod may be created;
  - which pod is deleted when the budget is full;
  - when held and idle pods end.
- **Sandbox service on Kubernetes.**
  - Create builds a pod from the operator's YAML template, fills in its name, labels, annotations and pod token, and returns once the pod's daemon has registered.
  - Delete deletes the pod.
  - It watches pod status and reports pods lost, with the reason.
- **Pool side** (unchanged in role).
  - Plays the Multica server toward each pod's stock daemon.
  - Hands a daemon's claim the task the core placed on its pod.
  - Relays the daemon's reports upstream with the account's PAT.
- **Pod.**
  - `tini` and the stock daemon, and nothing else of ours.
  - A liveness probe on the daemon's health endpoint.

### Lifecycle of a task

1. The policy picks the next department by fair share and checks the budget. It asks for a warm pod to exist, creating one if needed.
2. The sandbox service creates the pod and waits for its daemon to register.
   - If the pod is still pending, or its daemon has not registered, after the start timeout (default 3 minutes), the pod is deleted and creation backs off.
   - Nothing has been claimed for that pod.
3. The task source claims for that department's runtime of that kind.
4. The policy places the claimed task. The pod that was ready for the claim stays warm if the task goes elsewhere.
   - **Pool:** its key's held pod, then an idle pod of its department whose hold expired, then a warm pod, then a new pod.
   - **Clean:** its key's held pod, then a warm pod, then a new pod.
5. The pod's daemon claims from the pool side, gets the task, runs it and reports through the pool side.
6. When the task ends:
   - with a key, the pod is held for the department's hold timeout (default 10 minutes);
   - without one, a clean pod is deleted at once and a pool pod becomes idle.

   When the hold expires, a pool pod becomes idle and a clean pod is deleted.
7. An idle pool pod is deleted after its maximum idle lifetime (default 1 day).

### Policy rules

- **Key:** the task's issue, else its chat session. Tasks with neither have no key and are never held.
- **One task at a time per pod**, in both kinds.
- **A pod never serves another department.**
- **A clean pod never serves another key.** A pool pod may serve another key of its department once its hold has expired.
- **Warm pods** (default 2) are new pods that have served no one. They belong to no department or kind until their first task.
- **Budget:** `SANDBOX_MAX_PODS` counts warm, running, held and idle pods.
- **Fair sharing** keeps the guarantee, weight and quota, all counted in whole pods.
  - Occupancy is the department's running, held and idle pods.
  - Priority orders tasks within one department only.
- **When the budget is full and a pod is needed**, delete in this order:
  1. an idle pool pod;
  2. a held pool pod;
  3. a held clean pod.

  Across departments, delete from the department furthest over its share. Never delete a pod within a department's guarantee for another department.
- **A hold is never a guarantee.** Work that a later pod must see is pushed by the agent through the company's git MCP.

### State and restart

- Each pod carries labels:
  - `app.kubernetes.io/managed-by=sandbox-manager`;
  - the runtime kind;
  - the department.
- Its annotations record its key and its current task. The manager acts only on pods carrying its label.
- On start the manager lists its pods and rebuilds from them:
  - running tasks run on and report as before;
  - holds resume with their remaining time;
  - warm and idle pods are kept;
  - an unrecorded pod is kept as warm only if its containers never started; every other unrecorded pod is deleted. A record that fails to be written is retried until it is written or the pod is gone.
  - a task in the dispatch record whose pod is gone is failed.

### Failure reasons

A task whose pod fails is reported to Multica with the cause in words:
- out of memory, with the limit;
- evicted by the node;
- the daemon not answering;
- the pod deleted.

### Authentication to Kubernetes

- With a kubeconfig set, the manager uses it. For now that is the operator's own.
- Without one, it uses the in-cluster service account.
- The intended replacement is a dedicated service account with a namespace Role that allows only `create`, `delete`, `get`, `list`, `watch` and `patch` on pods (`patch` writes each pod's record). `deploy/` carries that YAML.
- When Kubernetes refuses the credentials, the manager stops claiming. Tasks stay queued in Multica, and the error shows on the dispatch page and in the log.
- When Kubernetes refuses `patch` on pods, the manager stops claiming the same way, until a record is written again: without records a restart would fail every running task.

### Settings

- **New:**
  - `SANDBOX_POD_TEMPLATE`: path of the pod YAML;
  - `SANDBOX_NAMESPACE`;
  - `KUBECONFIG`: optional;
  - `SANDBOX_RUNTIMES`: `pool`, `clean` or both; default `pool`;
  - `SANDBOX_MAX_PODS`;
  - `SANDBOX_WARM_PODS`: default 2;
  - `SANDBOX_POD_START_TIMEOUT`: default 3m;
  - `SANDBOX_POD_MAX_IDLE`: default 24h;
  - a name and device name for the clean runtimes, alongside `SANDBOX_MANAGER_NAME` and `SANDBOX_MANAGER_DEVICE_NAME`.
- **Kept:**
  - `MULTICA_SERVER_URL` and the pool token settings;
  - `SANDBOX_HOLD_TIMEOUT`;
  - `SANDBOX_POOL_QUOTA`;
  - `SANDBOX_WORKSPACE_OVERRIDES_FILE`, with quota, guarantee, weight and hold timeout;
  - `SANDBOX_PROVIDERS`, `SANDBOX_MODELS`;
  - the listen addresses, audit file, runtime visibility and sync intervals.
- **Removed:**
  - `SANDBOX_POOL_SIZE`;
  - `SANDBOX_POD_CAPACITY`;
  - `SANDBOX_PLACEMENT`, `SANDBOX_PLACEMENT_WAIT`;
  - `SANDBOX_POD_MAX_LIFETIME`, `SANDBOX_POD_DRAIN_GRACE`;
  - `SANDBOX_POOL_RESET_TIMEOUT`, `SANDBOX_POOL_HEARTBEAT_TIMEOUT`, `SANDBOX_POOL_LOST_RETENTION`, `SANDBOX_POOL_DRAIN_TIMEOUT`, `SANDBOX_POOL_LEASE_MAX`;
  - the pod endpoints `/pod/register` and `/pod/control`.

  An overrides file naming a removed setting, such as `rule`, is read with a warning.

  This list is checked against `cmd/sandbox-manager` when each setting's code goes.

### Admin API and dispatch page

- `/v1/inventory`, `/v1/leases` and `/v1/dispatch` keep the fields that still mean something.
- Pods gain their kind, department, key, phase and failure reason.
- Slots and bindings go away.
- The dispatch page shows each pod's kind and state: warm, running, held, idle, starting or failed. It shows pod creation failures with the reason, such as no node capacity or an image pull failure.

## Testing

- **The policy** is tested alone, with no fakes needed, on every placement and deletion case above.
- **The core** is tested with fakes of the task source, the sandbox service and the pool side.
- **The sandbox service** is tested against a fake Kubernetes client for template filling, readiness, the start timeout, losses and their reasons, and the rebuild from annotations.
- **End to end:** the scenarios run against a dedicated test namespace with real pods, the stock v0.6.4 daemon and a v0.6.4 backend, replacing the Docker Compose stack.
  - Locally that is `multica-sandbox` in the kind cluster `sandbox-dev`, which `deploy/k8s/dev/up.sh` creates with Multica inside it and a kubeconfig that may only create, delete, get, list and watch pods.
  - At the company, Multica, the manager and its pods share one namespace in the same way (`deploy/k8s/README.md`).

## Facts to establish while building

- The memory a pod uses idle (`tini`, daemon and agent CLI) against what tasks use. This sizes requests and limits, and the warm pod count.
- That a clean task's resume fails safely on a new pod. Multica still sends `PriorWorkDir` and a session. The daemon should fall back to a fresh environment (`execenv.Reuse` returns nil, and `gateResumeToReusedWorkdir` drops the session).
- That the pod template passes the namespace's admission policies (Pod Security, Kyverno or Gatekeeper, Istio injection).
- How the operator's kubeconfig authenticates (`kubectl config view --minify`). An `exec` plugin's token may expire.

## Out of scope

- Sizing pods by agent. Every pod has one size for now.
- Several tasks running in one pod.
- Keeping or syncing work outside the pod. Agents push through the git MCP.
- Replacing the stock daemon or changing Multica.
- Reading Multica's database.
