# A core between a task source and a pool side, sharing pods fairly

ADR 0007 gathered what the manager knows of Multica into `internal/multica`, but Multica still ran through the manager:
- workspaces, runtimes and their drain were the manager's state;
- the daemon protocol asked the manager routing questions;
- registering a workspace upstream hung on the first pod's registration.

Several facts were kept twice and repaired by reconciliation:
- the held pods against the sandbox service;
- the daemons' workspaces against the policy's;
- the claimed, queued and leased tasks across four maps.

Workspaces were not treated fairly. Claims followed one queue across workspaces, ordered by priority and then age. So:
- one workspace's burst took every free pod up to its quota;
- priorities set inside one workspace outranked another's;
- a pod was counted in slots, though affinity makes the whole pod one workspace's.

Instead the manager is three parts:

- **The core** (`internal/core`) decides which pod runs which task and keeps the one record of every task it dispatched, from claim to end. It knows tenants, opaque tasks and pod IDs, and imports only the lending policy. It reaches its two sides through the `TaskSource` and `Pool` interfaces in `ports.go`, so it is tested with fakes of them.
- **The task source** (`internal/multica/tasksource`) is the one daemon Multica sees. It:
  - follows the bot account's workspaces and gives the core each one as a tenant with its settings;
  - registers the runtimes, heartbeats them and recovers orphans;
  - claims and fails tasks when the core says so, and retries failure reports.
- **The pool side** (`internal/multica/pool`) lends the core pods:
  - it holds the sandbox service, the pod tokens and which workspaces each pod's daemon registered for;
  - it plays the Multica server toward the stock daemon;
  - it keeps no record of which task runs where. When a daemon claims, reports on a task or reads repos, it asks the core.

`internal/manager` only wires the three together and serves the admin API from what each reports.

The two sides pass to each other directly only what does not touch lending, wired in by the manager:
- the registration answer a pod's daemon needs;
- the model list requests, and the reports that answer them. The pods run one image, so the task source keeps a pod's report and answers the next requests of every runtime with the same agent CLI itself, for ten minutes; then a pod is asked again;
- the hints that send daemons to read their workspace list again.

Anything that changes a lease goes through the core: a task's end, a pod lost, a pod able to serve a workspace.

The task source heartbeats its runtimes on its own timer, every 15 seconds, for as long as it runs. It no longer relays the pods' heartbeats. A runtime is online whenever the manager runs; with no free pod nothing is claimed, so tasks stay queued in Multica (ADR 0001).

The lending policy shares pods between workspaces by **occupancy**: the whole pods a workspace's bindings hold, running or in hold, plus those reserved for its claims. Each workspace has three settings:
- a **guarantee**, kept for it even while it has nothing to run, scaled down in proportion when the guarantees exceed the live pods;
- a **weight** for the pods beyond the guarantees;
- its quota, as before.

The next claim goes to the workspace with the least occupancy beyond its guarantee per unit of weight. Priority orders tasks within one workspace only. Running tasks are never preempted: a workspace over its share loses its holds first, and never a hold within its guarantee.

## Considered options

- **Relay pod heartbeats upstream, as before.** It made "no pod" show as an offline runtime, but only as a side effect, and coupled the two sides through the core.
- **Register runtimes from configuration instead of from the first pod.** The agent CLIs and their versions are known only in the pod, and an empty `SANDBOX_PROVIDERS` must keep meaning "whatever the pods offer".
- **Read Multica's database to choose tasks.** Not needed: the pending API already returns the originator and the chat session, the fields affinity keys are read from. Choosing between workspaces is the manager's alone, since each has its own runtimes.
- **A guarantee others may borrow while it is idle.** Taking it back would either wait for the borrower's task, possibly for hours, or fail that task. A guarantee is kept, so it holds at once.

## Consequences

- The environment variables, the overrides file and every endpoint the pods call are unchanged. The overrides file gains the optional `guarantee` and `weight`; the inventory gains fields, and none changes meaning.
- A burst of tasks in one workspace no longer starves another, and priority is no longer compared across workspaces.
- Rebalancing between workspaces waits for bindings to end: at most the maximum lifetime plus the drain grace.
- A change to Multica's API is a change in `internal/multica`. A change in how pods run is a change in the pool side. The core changes only when lending does.
