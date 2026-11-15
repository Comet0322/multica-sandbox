# Architecture

The sandbox manager runs Multica tasks on pods it creates through the
Kubernetes API, one task at a time per pod. Toward Multica it is one ordinary
local daemon of one bot account; toward each pod it is the Multica server.
Multica and the daemon in the pods are stock: nothing in them knows the
manager exists.

```
Multica server ──/api/daemon/* (bot PAT)── task source ─┐
      ▲                                                  │
      │ task reports, agent calls (relayed)            core ── policy
      │                                                  │
      └──────────────────────────────────── pool side ──┴── sandbox service ── Kubernetes API
                                                │                                   │
                                                └── daemon protocol ── pods (tini + stock daemon + agent CLIs)
```

Terms follow [GLOSSARY.md](../GLOSSARY.md). How to run it is in the
[deployment guide](DEPLOYMENT.md).

## Components and their jobs

| Component | Package | Job | Knows nothing of |
| --- | --- | --- | --- |
| Task source | `internal/multica/tasksource` | The one daemon Multica sees. Follows the bot account's workspaces, registers a pool and a clean runtime set in each (each under its own daemon ID), heartbeats them, reads their pending lists, claims tasks and reports failures. | Pods, Kubernetes |
| Core | `internal/core` | Carries out the policy's decisions: acquire, claim, place, hold, delete, fail. Keeps the one record of every task it dispatched, from claim to end, and reconciles it with Multica. | Multica, Kubernetes (it sees tenants, opaque tasks and pod IDs) |
| Policy | `internal/policy` | Pure decisions: which department claims next, where a claimed task goes, whether a pod may be created, which pod goes when the budget is full, when holds and idle pods end. | Anything outside itself |
| Pool side | `internal/multica/pool` | Plays the Multica server toward each pod's daemon: authenticates it by its pod token, answers its registration, hands its claim the task the core placed on its pod, and relays its reports and its agents' calls upstream. | The policy |
| Sandbox service | `internal/sandbox` | Creates pods from the operator's pod template, watches them, keeps the warm pods, deletes pods that do not start in time, records on each pod what it serves, and reports lost pods with their reason. | Multica, workspaces, tasks |
| Manager | `internal/manager`, `cmd/sandbox-manager` | Wires the parts together, reads the settings, serves the admin API and the dispatch page. | |
| Pod | `deploy/Dockerfile.pool` | `tini` and the stock daemon, which runs one agent CLI per task as a non-root user. | The manager's internals |

The import rules that keep these apart are tested (`imports_test.go` in each
package; see [AGENTS.md](../AGENTS.md)).

## The life of a task

1. **A department is picked.** Every dispatch tick (1 second) the task source
   reads the pending list of each runtime in each workspace. The policy picks
   the department to claim for by fair share, then its oldest task of the
   highest priority.
2. **A pod is ready first.** Nothing is claimed without a ready pod: an idle
   pod of the department (pool runtime), else a warm pod. If there is none,
   the core asks the sandbox service for one, which creates it from the
   template; when the budget is full the policy names a pod to delete first.
   Until a pod is ready the task stays queued in Multica.
3. **The task is claimed** upstream for that department's runtime of that
   kind. Its **key** is its issue, else its chat session.
4. **The task is placed.** The policy places it by its kind:
   - pool: its key's held pod, else an idle pod of its department, else a
     warm pod, else a new pod;
   - clean: its key's held pod, else a warm pod, else a new pod.

   The pod readied for the claim stays warm if the task goes elsewhere.
5. **The pod runs it.** The core sends the pod's daemon a wake hint; the
   daemon claims from the pool side and gets that task, runs the agent CLI,
   and reports start, progress, messages and the result through the pool
   side, which relays them to Multica with the bot's PAT. The agent calls
   Multica with the task token Multica issued, through the manager.
6. **The task ends.** With a key, the pod is **held** for it for the
   department's hold timeout, so the issue's next task (the same agent again,
   or the next agent of a handoff) lands on it. Without a key, a pool runtime pod
   becomes idle at once and a clean pod is deleted.
7. **The hold ends.** A pool runtime pod becomes **idle**, free for any task of its
   department, and is deleted after its maximum idle lifetime; a clean pod is
   deleted.

A pod whose task cannot finish (out of memory, evicted, its daemon exited, the
pod deleted) is deleted, and the task is failed in Multica with the reason in
words. A restarted manager rebuilds its state from the labels and annotations
it wrote on its pods: running tasks run on, holds resume, warm and idle pods
are kept.

## Pool and clean runtimes

Each workspace sees up to two machines of the manager in its runtime list,
chosen with `SANDBOX_RUNTIMES`. Members choose by binding an agent to one.

| | Pool runtime | Clean runtime |
| --- | --- | --- |
| A task lands on | its key's held pod, an idle pod of its department, a warm pod, a new pod | its key's held pod, a warm pod, a new pod |
| After the hold | the pod is idle and may serve another key of the department | the pod is deleted |
| Sees earlier work of | its own issue, and of other issues of the department once their holds ended | its own issue only |
| Suits | ordinary work, handoffs within a squad | work that must not see or leave traces for other issues |

In both kinds a pod runs one task at a time and never serves another
department. A pod's daemon registers the runtimes of both kinds, since it may
be given a task of either.

## Fair sharing

Pods are shared between departments in whole pods. A department **occupies**
its running, held and idle pods. Each has, from the overrides file:

- a **guarantee**: pods kept for it even while it runs nothing; others may not
  take them;
- a **weight**: its share of the pods beyond the guarantees;
- a **quota**: the most pods its running tasks may take at once.

The next claim goes to the department with a queued task and room under its
quota that occupies the fewest pods beyond its guarantee per unit of weight;
between equals, the one whose next task has waited longest. Priority orders
tasks within one department only. Running tasks are never stopped to
rebalance: a department over its share gives pods back as its holds and idle
pods end or are deleted for the budget.

## Warm pods and the budget

`SANDBOX_MAX_PODS` is the **budget**: every pod counts, warm, starting,
running, held or idle. `SANDBOX_WARM_PODS` new pods that have served no one
are kept ready within it, so a claim seldom waits for a pod to start. A warm
pod belongs to no department or kind until its first task. At least one warm
pod is kept: the manager learns a workspace's runtimes from a pod's daemon
registering them.

When a pod is needed and the budget is full, the policy deletes, in order:

1. an idle pool runtime pod;
2. a held pool runtime pod;
3. a held clean pod;

taking it from the department furthest over its share, and never a pod within
another department's guarantee. A hold is therefore kept as long as possible,
never guaranteed.

A pod that has not started (its daemon has not called in) within
`SANDBOX_POD_START_TIMEOUT` is deleted, and pod creation backs off. Nothing was
claimed for it, so no task fails.

## What never reaches Multica

- **Pod details.** Pod names, IPs, node names and the pod count never appear
  in what the manager sends upstream: the runtimes carry the manager's own
  names, and pods run with the fixed hostname `sandbox`, so an agent printing
  its hostname shows that. The end-to-end scenarios check this
  (`deploy/k8s/e2e`).
- **Pod tokens.** Each pod's token is known only to the manager and that pod.
- **Kubernetes errors as such.** Multica sees a failed task's reason in words
  (`the pod ran out of memory (limit 4Gi)`), not Kubernetes objects.
- **The pods' heartbeats.** The manager heartbeats its runtimes itself; a
  runtime is online while the manager runs, pods or not.

In the other direction, the bot's PAT never reaches a pod, and nothing about
who started a task is written on a pod. The manager handles no repository or
LLM credentials: agents bring their own in their custom environment in
Multica, which the daemon passes to the agent unchanged.

## Design trade-offs

| Choice | What it gives | What it costs |
| --- | --- | --- |
| One task per pod | Each task gets its own memory limit; one task running out of memory ends only itself. Kubernetes packs pods onto nodes by their requests. | A pod's fixed overhead (the daemon and `tini`) per running task, and a pod start when no ready pod is at hand. |
| The manager creates pods through the Kubernetes API | Pods are sized and placed by Kubernetes; the pod count follows demand within the budget. | The manager needs rights on pods in its namespace, and the namespace's admission policies apply to every pod. |
| Pods are deleted, never cleaned for reuse | Nothing of a pod outlives it; no reset logic to trust. | Work an agent did not push is lost with the pod. |
| Holds by key | A handoff within a squad finds the previous agent's unpushed work while the pod is held. | Held pods count against the budget, and a hold may be dropped when the budget is full. |
| Pool runtime pods shared within a department | Fewer pod starts; an issue's later tasks may reuse a warm cache. | Another issue of the department may see files left on the pod; the clean runtime exists for work that must not. |
| Warm pods | Claims rarely wait for a pod to start. | Pods that use their requests while serving no one. |
| State kept on the pods (labels and annotations) | The manager keeps no database; a restart takes over running tasks. | It needs the `patch` verb on pods; without it the manager stops claiming, and a restart deletes every pod. |
| Stock Multica and stock daemon | No fork to maintain; the manager follows Multica's daemon protocol. | The manager depends on that protocol (pinned to v0.6.4 by fixtures) and on Multica's API for workspaces and tasks. |
| One bot account for every workspace | One credential, one daemon identity, workspaces found by membership. | The PAT is a single point of failure, and a workspace that invites the bot is served by it. |
