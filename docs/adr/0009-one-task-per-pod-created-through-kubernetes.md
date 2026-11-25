# One task per pod, created through the Kubernetes API

The company runs stock Multica v0.6.4 for 16 departments, about 1,400 people. Each department's squad runs on one runtime, which is one pod. Two problems come from that:

- **Out of memory.** A runtime runs every task of its department in one daemon, with no limit per task. One task that runs out of memory takes down the daemon and every other task on it. The only knob is the daemon's concurrency, which is a guess.
- **Handoff.** A second runtime does not help. A squad hands an issue from one agent to the next, and the next agent checks out the previous agent's branch from the machine's shared repo cache. The previous agent has not pushed it, so on another machine the branch is not there.

Multica expects each team to bring its own machines, with each agent bound to one of them. It has no pool of runtimes and no scheduling between them.

ADR 0001 to 0008 built the manager on three premises:
- pods are created by GitOps, so their number is fixed and the manager can only reuse them, resetting them in between;
- a pod is shared by several tasks;
- what a handoff needs stays on the pod.

Those premises made a pod's free capacity depend on which task comes next, but a claim cannot choose its task. That is the source of:
- bindings and affinity keys;
- holds, eviction, maximum lifetime and drain grace;
- reserving before a claim and correcting after it;
- slots;
- the pod supervisor's reset.

## Decision

The manager creates and deletes pods itself through the Kubernetes API, in one namespace, and **a pod runs one task at a time**.

- **The sandbox service creates pods.** Create builds a pod from a YAML template the operator provides (`SANDBOX_POD_TEMPLATE`). It fills in the name, labels, annotations and the pod token, waits until the pod's daemon has registered, and returns the pod. Delete deletes it. Nothing is reset. PutFiles, GetFiles and Exec stay unsupported.
- **The pod supervisor is removed.**
  - The container runs `tini` and the stock daemon.
  - The sandbox service watches pod status in place of the control channel.
  - The pod token is set in the pod's environment when the pod is created, in place of the registration endpoint.
  - A pod whose daemon fails is not restarted. Its task fails and the pod is deleted.
- **Memory is limited per task** by the pod's own limits, so running out of memory ends that task only. Its failure reason says so, for example `pod ran out of memory (limit 4Gi)`.
- **A hold keeps a pod for its key.** After its task ends, a pod is held for the task's issue or chat session for the department's hold timeout (default 10 minutes). The next task with the same key goes to that pod. A hold is only kept as long as possible, never guaranteed:
  - it is dropped when the budget is full;
  - agents that must hand work to a later pod push it themselves through the company's git MCP.

  Tasks with neither an issue nor a chat session get no hold.
- **Two runtime kinds**, chosen by `SANDBOX_RUNTIMES` (`pool`, `clean` or both; default `pool`). Each kind registers its own runtimes in every workspace under its own daemon ID, so members choose by binding an agent to one.
  - A **pool** task takes, in order:
    1. its key's held pod;
    2. an idle pod of its department whose hold expired;
    3. a warm pod;
    4. a new pod.

    An idle pool pod is deleted after a maximum idle lifetime (default 1 day).
  - A **clean** task takes:
    1. its key's held pod;
    2. a warm pod;
    3. a new pod.

    A clean pod never serves another key, and its hold's expiry deletes it.

  In both kinds a pod never serves another department.
- **Warm pods.** The manager keeps a few new pods (default 2) that have not served anyone. They belong to no department or kind until their first task.
- **The budget is a pod count**, `SANDBOX_MAX_PODS`, counting warm, running, held and idle pods. Fair sharing (guarantee, weight, quota) is unchanged and counts whole pods.
  - When a new pod is needed and the budget is full, the manager first deletes an idle or held pool pod, then a held clean pod.
  - Across departments it deletes from the one furthest over its share.
- **A pod is ready before anything is claimed for it.** A pod still pending, or whose daemon has not registered within the start timeout (default 3 minutes), is deleted, and pod creation backs off. Nothing was claimed for it, so no task fails; tasks stay queued in Multica (ADR 0001).
- **Kubernetes holds the state.** Each pod's annotations record its department, kind, key and current task. On start the manager lists its own pods and rebuilds from them:
  - running tasks run on;
  - holds resume;
  - a task whose pod is gone is failed.
- **The lending policy becomes `internal/policy`.** It stays pure and decides:
  - which department claims next;
  - where a claimed task goes;
  - whether a pod may be created;
  - which pod is deleted when the budget is full;
  - when held and idle pods end.
- **The manager authenticates to Kubernetes** with a kubeconfig when one is set, which for now is the operator's own. Without one it uses the in-cluster service account. A dedicated service account with a namespace Role that only allows `create`, `delete`, `get`, `list`, `watch` and `patch` on pods (`patch` writes each pod's record) is the intended replacement, with no code change.
  - When Kubernetes refuses the manager's credentials, it stops claiming, so tasks stay queued in Multica, and it reports the error on the dispatch page.
  - When Kubernetes refuses `patch` on pods, it stops claiming the same way: without records a restart would fail every running task.

What stays: the stock daemon and stock Multica, the task source, the pool side's daemon protocol, the core's dispatch record, fair sharing, and the rule that pods' details never reach Multica.

## Considered options

- **Keep GitOps pods and reset them, one task per pod.** Rejected by the operator. It keeps a fixed pod size and the supervisor's reset, and the manager would still not size or place pods.
- **Share a pod between several running tasks.** It packs nodes tighter, but one task's memory takes down the others again. Kubernetes packs one-task pods by their requests instead.
- **Sync handoff branches to an internal git remote from the pod.** Rejected. The company's git MCP already lets agents push, and that leaves out of git what should stay out.
- **Read Multica's database to predict the next task.** Not needed once a pod's capacity no longer depends on the task. ADR 0001's rule against database access stands.
- **Replace the stock daemon in the pod.** It is about 84,000 lines covering about 20 agent CLIs and changes weekly. With one task per pod its weaknesses no longer matter.

## Consequences

- Supersedes ADR 0001's reservation of a held pod's slot. What remains of reserve-then-claim is that a ready pod exists before each claim.
- Supersedes ADR 0003's registration endpoint. Pod tokens are issued when the pod is created, so nothing that can reach the manager can register as a pod any more.
- Refines ADR 0004: Create and Delete now provision and delete real pods.
- Closes Open 1 of the task-source-and-pool spec: one task per pod, with no state kept outside the pod by the manager.
- Removed:
  - the pod supervisor and its endpoints (`/pod/register`, `/pod/control`);
  - slots and pod capacity;
  - bindings, affinity rules and the person in a key;
  - eviction waits and placement wait;
  - maximum lifetime and drain grace;
  - the GitOps pool manifests.

  The settings for them are listed in the deployment guide. An overrides file that still names a removed setting, such as `rule`, is read with a warning.
- Kept: quota, guarantee, weight and hold timeout in the overrides file, the admin API fields that still mean something, and every endpoint the daemon calls.
- Within a department, different people's pool tasks may use the same pod one after another and see each other's files. Work that needs isolation uses agents bound to a clean runtime.
- A handoff across pods works only for what the previous agent pushed.
- End-to-end scenarios run against a dedicated test namespace instead of Docker Compose.
