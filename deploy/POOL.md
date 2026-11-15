# Sandbox manager: deployment guide

`sandbox-manager` is a normal Multica account's local daemon: it serves every
workspace that account belongs to, finds new ones by itself, and needs no
workspace list (SIGHUP looks again at once). It runs every task on a pod of its
own, which it creates through the Kubernetes API from the operator's pod
template and deletes when the pod is no longer needed (ADR 0009).

The settings are read once at startup; a bad value stops the process with a
message naming the variable. The exception is the
[per-workspace overrides](#per-workspace-overrides) file, which is read on
every sync and whose problems are reported without stopping anything.
Durations use Go syntax (`45s`, `10m`, `2h`). Unset or `0` means the default
shown, unless the table says otherwise.

## Modules, one process

- The **sandbox service** (`internal/sandbox`) owns the pods: it creates them
  from the pod template with a pod token in their environment, watches their
  status, keeps the warm pods, deletes pods that do not start in time,
  records on each pod what it serves, and deletes pods. It knows nothing of
  Multica, workspaces or tasks.
- The **core** (`internal/core`) carries out the policy's decisions and keeps
  the record of every task dispatched. The **policy** (`internal/policy`:
  fair share, quota, placement by runtime kind and key, holds, idle lifetime,
  budget deletion) knows nothing of Kubernetes or Multica.
- The **task source** (`internal/multica/tasksource`) is the one daemon
  Multica sees; the **pool side** (`internal/multica/pool`) plays the Multica
  server toward each pod's stock daemon.

The core reaches pods only through the service: `Create` takes a ready pod no
one holds and `Delete` deletes it. `PutFiles`, `GetFiles` and `Exec` exist on
the interface but are not supported. See ADR 0004
(`docs/adr/0004-pods-behind-a-sandbox-service.md`) for the seam.

Secrets accept `NAME` or `NAME_FILE` (a path; surrounding whitespace is
trimmed). Setting both is refused. Secret values are never logged or echoed in
errors. Prefer the `_FILE` form with a mounted secret.

## Settings

### Upstream and identity

| Variable | Default | Meaning |
| --- | --- | --- |
| `MULTICA_SERVER_URL` | none | Multica server base URL. Required. |
| `SANDBOX_POOL_TOKEN` | none | PAT of the account the manager runs as (secret; `_FILE` form accepted). Required. Use a dedicated account that is a member of every workspace to serve. It is the only Multica credential (see [Known limits](#known-limits-and-operations)). |
| `SANDBOX_MANAGER_NAME` | `Managed Sandbox` | Machine name of the pool runtimes: the title of the manager's entry in Multica's runtime list. Each runtime is named `<agent CLI> (<name>)`. No parentheses or ` · `. A rename in the UI wins over it. |
| `SANDBOX_MANAGER_DEVICE_NAME` | `SANDBOX_MANAGER_NAME` | The line under the title (the registration's `device_name`). Both names reach Multica when a workspace registers, so after a manager restart. |
| `SANDBOX_MANAGER_CLEAN_NAME` | `SANDBOX_MANAGER_NAME` followed by ` Clean` | Machine name of the clean runtimes. |
| `SANDBOX_MANAGER_CLEAN_DEVICE_NAME` | the clean name | Shown under it. |
| `SANDBOX_MANAGER_DAEMON_ID` | derived | Daemon ID of the pool runtimes in every workspace; the clean runtimes use it followed by `-clean`. Unset, it is derived from `MULTICA_SERVER_URL` and the bot account, the same on every restart. Changing it registers new runtimes. |
| `SANDBOX_POOL_RUNTIME_VISIBILITY` | `public` | Visibility set on the runtimes registered in each workspace. A runtime starts private to its owner, so `public` is what lets every member bind agents to it; `private` leaves sharing to the owner. |
| `SANDBOX_PROVIDERS` | all | Comma list of agent CLIs exposed as runtimes. Set it to the image's CLIs: a pod could otherwise add runtimes of any type. |
| `SANDBOX_MODELS` | none | Models the agent settings picker offers, per agent CLI: `claude=claude-sonnet-4-6,claude-opus-4-7;codex=gpt-5.2`, the first of each list the default. The manager answers the picker from a pod's latest report, for ten minutes; these lists answer it until a pod has reported. Unset, the picker waits for a pod. |
| `SANDBOX_POOL_SYNC_INTERVAL` | `30s` | How often the account's workspaces are listed again. A workspace joined is served from the next pass; one left out of two listings in a row drains like a removed one, and one that comes back while draining is served again with its leases intact. A failed or empty listing changes nothing. |

### Pods

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_POD_TEMPLATE` | none | Absolute path of the pod YAML every pod is created from (see [k8s/README.md](k8s/README.md) and `k8s/pod.yaml`). Required. One Pod, with the daemon's container named `daemon` or as its only container; unknown fields are refused. |
| `SANDBOX_NAMESPACE` | the kubeconfig context's, else the pod's own | Namespace the pods are created in. |
| `KUBECONFIG` | none | Kubeconfig to reach Kubernetes with (for now, the operator's own). Unset, the manager uses its in-cluster service account. |
| `SANDBOX_RUNTIMES` | `pool` | The runtime kinds registered in every workspace: `pool`, `clean` or `pool,clean` (see [Runtime kinds](#runtime-kinds)). Removing a kind stops heartbeating its runtimes, so they show offline. |
| `SANDBOX_MAX_PODS` | `10` | The budget: the most pods at once, warm, starting, running, held and idle. |
| `SANDBOX_WARM_PODS` | `2` | Ready pods kept that have served no one; `0` keeps none (a pod is then created when a claim needs one). At most `SANDBOX_MAX_PODS`. |
| `SANDBOX_POD_START_TIMEOUT` | `3m` | How long a new pod may take until its daemon calls the manager. Past it the pod is deleted, and pod creation backs off (10s, doubling up to 5m). Nothing was claimed for it, so no task fails. |
| `SANDBOX_HOLD_TIMEOUT` | `10m` | How long a pod whose task ended is held for the task's key. `0` holds nothing. Overridable per workspace. |
| `SANDBOX_POD_MAX_IDLE` | `24h` | How long an idle pool pod is kept before it is deleted. |
| `SANDBOX_POOL_QUOTA` | `0` | The most pods one workspace's running tasks may occupy at once; `0` is no cap. Overridable per workspace. |
| `SANDBOX_WORKSPACE_OVERRIDES_FILE` | none | Absolute path of the [per-workspace overrides](#per-workspace-overrides) file, read again on every sync. |

### Listeners, dispatch and audit

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_MANAGER_LISTEN` | `:8790` | Pod API listener. Also serves `/healthz` (liveness: always 200) and `/readyz` (200 only if a workspace listing succeeded within three sync intervals, else 503). |
| `SANDBOX_POOL_ADMIN_LISTEN` | `127.0.0.1:8791` | Admin listener (see [Admin API](#admin-api-and-dispatch-page)). Not for pods. |
| `SANDBOX_POOL_WAKE_INTERVAL` | `1s` | Dispatch tick. `0` does not turn dispatch off: the tick then runs every second and only the wake socket hints are disabled. |
| `SANDBOX_POOL_RECONCILE_INTERVAL` | `30s` | Lease reconciliation period; `0` turns it off (and with it the retry of failure reports). See [Lease end and reconciliation](#lease-end-and-reconciliation). |
| `SANDBOX_AUDIT_FILE` | off | JSON Lines audit log, appended (0600). `-` writes to stdout. |

The manager handles no repository or LLM credentials. Agents reach an LLM and
a git host with their own custom environment in Multica, which the daemon
passes to the agent unchanged.

### Removed settings and endpoints

These belonged to the pool of pods that were started outside the manager and
reset between tasks, before ADR 0009. Each one still set is ignored, with a
warning at start (`<NAME> is no longer used (ADR 0009) and is ignored`):

| Variable | What it was |
| --- | --- |
| `SANDBOX_POOL_SIZE` | The declared pod count; the budget is `SANDBOX_MAX_PODS`. |
| `SANDBOX_POD_CAPACITY` | Tasks per pod; a pod runs one task at a time. |
| `SANDBOX_PLACEMENT`, `SANDBOX_PLACEMENT_WAIT` | The affinity rule and the wait for a pod after an eviction; a task's key is its issue, else its chat session. |
| `SANDBOX_POD_MAX_LIFETIME`, `SANDBOX_POD_DRAIN_GRACE` | A pod's maximum lifetime and its drain grace. |
| `SANDBOX_POOL_RESET_TIMEOUT`, `SANDBOX_POOL_HEARTBEAT_TIMEOUT`, `SANDBOX_POOL_LOST_RETENTION` | The reset and liveness of registered pods; pods are watched through Kubernetes. |
| `SANDBOX_POOL_DRAIN_TIMEOUT`, `SANDBOX_POOL_LEASE_MAX` | Now fixed: a removed workspace keeps its leases for 10 minutes, and a lease lasts at most 2 hours. |

The pod endpoints `/pod/register` and `/pod/control` are gone: a pod's token
is set in its environment when the manager creates it, and its status comes
from Kubernetes. The stock daemon never called them.

In the overrides file, `placement`, `rule` and `max_lifetime` are read with
one warning per workspace entry (listed in `override_problems`) and ignored.

## Runtime kinds

Each kind registers its own runtimes in every workspace, as a machine of its
own upstream, so members choose by binding an agent to one.

A task's **key** is its issue, else its chat session; a task with neither has
no key and is never held. A pod runs one task at a time and never serves
another department (workspace).

- **Pool runtime.** A task takes its key's held pod, else an idle pod of its
  department, else a warm pod, else a new pod. Pool pods are shared within a
  department: once a pod's hold ended, a task of another key, even another
  person's, may land on it and sees what earlier tasks left there.
- **Clean runtime.** A task takes its key's held pod, else a warm pod, else a
  new pod. A clean pod never serves another key.

When a task ends, its pod is held for the task's key for the hold timeout;
without a key, a pool pod becomes idle at once and a clean pod is deleted.
When a hold ends, a pool pod becomes idle and a clean pod is deleted. An idle
pool pod is deleted after `SANDBOX_POD_MAX_IDLE`.

A **hold** is kept as long as possible but never guaranteed: when the budget is
full and a pod is needed, the manager deletes an idle pool pod first, then a
held pool pod, then a held clean pod, from the department furthest over its
share, and never one within another department's guarantee. A handoff across
pods (agent A hands an issue to agent B, and B lands on another pod) sees only
what A pushed, through the company's git MCP for example; anything A left
unpushed stays on A's pod.

A **warm pod** is new and ready and belongs to no department or kind until its
first task. Before each claim the manager makes sure a ready pod exists (a
warm pod, or an idle pod of the department for a pool task), so a claimed task
always has somewhere to run; a task no pod can take stays queued in Multica.

## Pods and their failures

The manager acts only on pods labelled
`app.kubernetes.io/managed-by=sandbox-manager`. It fills in each pod's name,
labels, annotations, restart policy (`Never`), pod token and, when the
template sets none, the hostname `sandbox`. A pod is ready once its daemon
first calls the manager with its token.

A pod that cannot run any more fails its task, which Multica shows with the
reason in words, and the pod is deleted:

| Cause | Reason |
| --- | --- |
| The pod's container ran out of memory | `the pod ran out of memory (limit <limit>)` |
| The node evicted the pod | `the node evicted the pod: <message>` |
| The daemon's container exited (a failing liveness probe ends this way) | `the pod's <container> container exited with code <code> (<reason>)` |
| The pod was deleted outside the manager | `the pod was deleted` |

The task fails as `the pod running this task was lost: <reason>`. A pod that
does not start in time shows its waiting reason (no node capacity, an image
that cannot be pulled) in the inventory's and the dispatch page's `problem`.

When Kubernetes refuses the manager's credentials, no pod can be created, so
nothing is claimed: tasks stay queued in Multica, and the error shows as the
`problem` and in the log.

The manager records on each pod what it serves (patching its labels and
annotations), and a restarted manager rebuilds its state from them: see
[What a pod serves, and a manager restart](k8s/README.md#what-a-pod-serves-and-a-manager-restart).
Recording needs the `patch` verb on pods.

## Pod API authorization

Pods are identified by their pod token only, never by their address, so a
mesh that rewrites source addresses changes nothing.

- Every daemon call needs a pod token of a pod the manager created; anything
  else gets 401. That includes the workspace list (`/api/workspaces`,
  `/api/daemon/workspaces`). Agent CLI calls with a `mat_` task token outside
  the daemon protocol are relayed to Multica as they are; a pod token there
  gets 403.
- A pod's task reports are relayed only for the task the manager placed on
  that pod.
- A workspace's repos and settings (`/api/daemon/workspaces/{id}/repos`),
  which the manager reads with its PAT, are served only to a pod that holds a
  task of that workspace, or to a warm pod registered for the workspace. A pod
  working for another workspace gets 403.
- A pod heartbeating a runtime the manager does not know, or of a workspace it
  has not registered for since the manager started, gets
  `404 runtime not found`, which makes the stock daemon register again.

The manager heartbeats its registered runtimes upstream itself, every 15
seconds, for as long as it runs; the pods' heartbeats are answered by the
manager and never relayed. A runtime shows online whenever the manager runs,
pods or not.

## Lease end and reconciliation

A lease ends in these cases; `end_reason` in `/v1/leases` says which:

| Cause | `end_reason` | Task failed upstream? |
| --- | --- | --- |
| The pod reports `complete`, `fail` or `cancel-ack` and Multica takes it | `pod reported <action>` | no (the pod reported it) |
| Multica refuses that report with 401, 403 or 404 | `upstream refused the pod's <action> report (HTTP <code>)` | no (Multica no longer knows the task) |
| Reconciliation finds the task completed, failed or cancelled | `upstream task ended: <status>` | no |
| Reconciliation gets 404 for the task | `upstream task ended: not found` | no |
| Reconciliation gets 401 or 403 | `upstream access lost (HTTP <code>)` | no (it cannot be) |
| The lease is older than 2 hours | `lease exceeded the maximum lease time` | yes: "task exceeded the maximum lease time" |
| The pod was lost (see above) | `pod lost` | yes, with the pod's reason |
| Its workspace was removed and 10 minutes passed | `workspace removed from the configuration` | yes |

Reconciliation runs every `SANDBOX_POOL_RECONCILE_INTERVAL`: it asks Multica
for each leased task's status with the endpoint the stock daemon polls
(`GET /api/daemon/tasks/{id}/status`, with the PAT), at most 8 calls at once
and 10s each. Any other error keeps the lease for the next pass.

A failure report that does not reach Multica (network error, timeout, 429 or
5xx) is queued and sent again by every reconciliation pass until Multica takes
it, refuses it with another 4xx (taken as already handled), or 30 minutes have
passed. `/v1/inventory` shows the queue length as `pending_fail_reports`.

## Per-workspace overrides

The hold timeout and quota above are pool defaults, and every workspace has a
guarantee of 0 and a weight of 1. `SANDBOX_WORKSPACE_OVERRIDES_FILE` names a
file (an absolute path) that overrides them for single workspaces, keyed by
workspace slug.

```yaml
# overrides.yaml: YAML or JSON, one entry per workspace slug.
acme:
  hold_timeout: 1h         # SANDBOX_HOLD_TIMEOUT; 0 holds nothing
  quota: 6                 # SANDBOX_POOL_QUOTA; 0 is no cap
  guarantee: 2             # pods kept for it; 0 (the default) keeps none
  weight: 2                # its share of the pods beyond the guarantees; 1 by default
sandbox-lab:
  hold_timeout: 0
```

A field left out keeps the pool default. A workspace without an entry uses
the pool defaults.

The file is read again on every sync and on SIGHUP, so an edit applies without
a restart. A changed setting applies to new claims and placements only: a hold
keeps its end, and a quota lowered below the running tasks only stops new
claims. The workspace-reloaded log line lists the changed workspaces as
`settings_updated`.

Problems never stop the manager or the other workspaces. Each one is logged
once, as a WARN `workspace override not applied` line, when the set of
problems changes, and is listed in `override_problems` in `/v1/inventory`
until it is fixed:

- an entry with an unknown field or a bad value is left out. The workspace
  keeps its previous override, or the pool defaults when it had none;
- an entry naming a removed setting (`placement`, `rule`, `max_lifetime`) is
  applied without it, with one problem for the entry;
- a slug that no workspace of the account has is ignored;
- a file that cannot be read or parsed keeps every override in effect.

Slugs come from the server's member listing (`GET /api/workspaces`), which is
read only while the file has entries.

## Task dispatch and fair sharing

The stock daemon in a pod polls for a task every 30 seconds, so the manager
dispatches itself: the daemon opens its usual wake websocket on the manager,
and every `SANDBOX_POOL_WAKE_INTERVAL` the manager reads the queue, makes sure
a ready pod exists, claims one task upstream, places it where the policy says,
and sends the target pod the hint that makes it claim at once. Claims are made
only on the tick; a pod's poll receives only a task already placed on it.

The manager shares the pods between workspaces (departments), counted in whole
pods: a workspace **occupies** its running, held and idle pods.

1. Every dispatch tick it reads the pending list of each of its runtimes in
   each served workspace (`GET /api/daemon/runtimes/{id}/tasks/pending`) and
   keeps the `queued` tasks.
2. Among the workspaces with a queued task and room under their quota (which
   counts running tasks and claims in flight, not held or idle pods), it picks
   the one with the fewest pods occupied beyond its `guarantee`, per unit of
   `weight`; between equals, the one whose next task has waited longest.
   Priority orders tasks within one workspace only.
3. A workspace's **guarantee** is kept for it: while it occupies fewer pods
   than its guarantee, other workspaces may not take the rest, even when it
   has nothing to run. The pods beyond the guarantees go by **weight**. When
   the guarantees add up to more than the live pods, each is scaled down in
   proportion.
4. Running tasks are never stopped to rebalance; a workspace over its share
   gives pods back as its holds and idle pods end or are deleted for the
   budget.

## Admin API and dispatch page

The admin listener serves, read-only:

- **`GET /v1/inventory`**: `max_pods` (the budget); the pod counts `warm`,
  `starting`, `running`, `held`, `idle` and `failed`; `pruned_total` (failed
  pods dropped from the list after 10 minutes); `problem`, why pods could not
  be created or started lately; `live_pods`; `last_sync`;
  `oldest_lease_age_seconds`; `pending_fail_reports`; `override_problems`;
  each pod with its `id`, `name`, `phase` (`warm`, `starting`, `running`,
  `held`, `idle` or `failed`), `kind`, `workspace_id`, `key`, `task_id`,
  `initiators`, `created_at`, `last_seen`, `hold_until`,
  `hold_remaining_seconds`, `idle_since` and `reason` (why a failed pod
  failed, or why a starting pod still waits); and each workspace (`workspaces`,
  then `draining`) with its `slug`, `name`, `leased`, `quota`,
  `hold_timeout`, `guarantee`, `guarantee_in_effect`, `weight`,
  `occupied_pods`, `holding_pods`, `idle_pods`, `target_pods`, `queued` and
  `upstream` (its latest claim or heartbeat to Multica).
- **`GET /v1/leases`**: active and recently closed leases, oldest first, with
  `task_id`, `workspace_id`, `initiator`, `pod_id`, `started_at`, `ended_at`
  and `end_reason`.
- **`GET /v1/dispatch`**: `pool` (`max`, `live`, `warm`, `starting`,
  `running`, `held`, `idle`, `failed`, `problem`), each pod (`id`, `name`,
  `phase`, `kind`, `workspace_id`, `key`, `task_id`,
  `hold_remaining_seconds`, `initiators`, `reason`), each workspace's share
  and its tasks (queued, starting, running, recently ended), and totals.
- **`GET /dispatch`**: a page that shows `/v1/dispatch` and reads it again
  every 10 seconds: each pod's kind and phase, pod creation failures with
  their reason, and each workspace's pods against its target.

`/readyz` on the pod listener turns 503 when no listing succeeded for three
sync intervals: a revoked or expired PAT, or Multica unreachable. Point a
readiness probe at it, but keep publishing a not-ready manager to its pods
(`publishNotReadyAddresses`), or their running tasks could not report.

Pod deletions and leases are audited (`action: delete`, `lease`, `release`),
with the task that caused them.

## Multica server version

The manager needs Multica v0.6.4 or later and no migration newer than it. It
calls, with the bot PAT: `GET /api/me`, `GET /api/daemon/workspaces` (and
`GET /api/workspaces` for slugs while the overrides file has entries),
`POST /api/daemon/register`, `POST /api/daemon/heartbeat`,
`POST /api/daemon/runtimes/{id}/recover-orphans`,
`GET /api/daemon/runtimes/{id}/tasks/pending`, `POST /api/daemon/tasks/claim`,
`GET /api/daemon/tasks/{id}/status`, `POST /api/daemon/tasks/{id}/fail`,
`GET /api/runtimes` and `PATCH /api/runtimes/{id}`; it relays the pods' task
calls (`start`, `progress`, `session`, `complete`, ...) and the agents'
`mat_` calls.

## Known limits and operations

- **A pod deleted while the manager is down.** A restarted manager keeps the
  running tasks of the pods it finds. A task whose pod was deleted while the
  manager was down is not in Kubernetes any more and Multica shows it running,
  so it waits for Multica's own timeout.
- **Without the right to patch pods, a restart fails every running task.**
  Nothing is recorded on the pods, so a restarted manager cannot tell a warm
  pod from one that served someone: it deletes every pod it finds, and the
  tasks that ran on them are failed upstream (Multica retries those eligible).
- **Memory is per pod.** The pod template's limits bound each task; a task
  over them fails with the out-of-memory reason, and only that task.
- **Pool pods are shared within a department.** Work that must not be seen by
  another key of the department uses an agent bound to a clean runtime.
- **Accepting an invitation opens the manager to that workspace.** The manager
  serves every workspace its account belongs to, and its runtimes are `public`
  by default, so every member of a workspace the bot joins can bind agents to
  them within one sync interval. Set `SANDBOX_POOL_QUOTA`, guarantees and
  weights so one workspace cannot take every pod.
- **One daemon identity per bot account.** Two managers with the same account
  against the same Multica register the same runtimes unless
  `SANDBOX_MANAGER_DAEMON_ID` keeps them apart. Run one manager per namespace:
  two would act on the same labelled pods.
- **Claim load and dispatch rate.** Every tick reads one pending list per
  runtime of every served workspace, and hands out at most one task, so
  filling many pods after a burst takes about one second per task.
- **The PAT is a single point of failure.** It acts in every served workspace,
  and when it is revoked or expires every workspace stops at once: new tasks
  stay queued in Multica, reports of running tasks are refused and their
  leases end, `/readyz` turns 503, and `last_sync` and the workspaces'
  `upstream` show the error. To rotate it, create the new PAT, replace the
  Secret file, restart the manager (running tasks carry on, see above), then
  revoke the old PAT.
- **Restrict who reaches the manager's pod port.** A pod token is needed for
  every daemon call, but anything inside a pod holds its token; keep other
  callers away from the pod listener with the namespace's network policy.
