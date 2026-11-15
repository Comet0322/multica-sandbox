# Sandbox manager configuration

`sandbox-manager` runs as the pool manager when `SANDBOX_POOL_TOKEN` (or
`SANDBOX_POOL_TOKEN_FILE`) is set. The manager is a normal Multica account's
local daemon: it serves every workspace that account belongs to, finds new ones
by itself, and needs no workspace list (SIGHUP looks again at once). The settings are read once at startup; a bad
value stops the process with a message naming the variable. The one exception
is the [per-workspace overrides](#per-workspace-overrides) file, which is read
on every sync and whose problems are reported without stopping anything. Durations use Go
syntax (`45s`, `10m`, `2h`). Unset or `0` means the default shown.

## Modules, one process

The program runs in one process with one pod listener:

- The **sandbox service** (`internal/sandbox`) owns the
  pods: registration and pod tokens, the control channel, resets, liveness,
  pruning, and the set of free pods. It knows nothing of Multica, workspaces
  or tasks. Its settings are `SANDBOX_POOL_HEARTBEAT_TIMEOUT`,
  `SANDBOX_POOL_RESET_TIMEOUT` and `SANDBOX_POOL_LOST_RETENTION`.
- The **manager** (`internal/manager`) is everything else: workspaces, claims,
  bindings, hold, quota, leases and the audit of leases. It reaches pods only
  through the service: `Create` takes a free, reset pod and `Delete` hands it
  back to be reset. `PutFiles`, `GetFiles` and `Exec` exist on the interface
  but the current pods do not support them. The service tells it when a pod it
  holds is interrupted or lost. Two packages hold its parts:
  - `internal/policy`, the policy (queue, fair share, quota, placement by
    runtime kind and key, holds, idle lifetime, budget deletion), which
    knows nothing of Kubernetes or Multica: a workspace is a tenant to it.
  - `internal/multica`, everything the manager knows of Multica: the server's
    API, the daemon protocol it serves to the pods, runtime registration and
    the affinity rules.

Every other setting below belongs to the manager. See ADR 0004
(`docs/adr/0004-pods-behind-a-sandbox-service.md`) for the seam.

Secrets accept `NAME` or `NAME_FILE` (a path; surrounding whitespace is
trimmed). Setting both is refused. Secret values are never logged or echoed in
errors. Prefer the `_FILE` form with a mounted secret.

## Core

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_POOL_TOKEN` | none | PAT of the account the manager runs as (secret; `_FILE` form accepted). Selects pool mode. Use a dedicated account that is a member of every workspace to serve. It is the only Multica credential: every workspace is served with it (see [Known limits](#known-limits-and-operations)). |
| `SANDBOX_POOL_QUOTA` | `0` | The quota: the most slots one workspace's running tasks may occupy at once; 0 is no cap. Overridable per workspace. |
| `SANDBOX_WORKSPACE_OVERRIDES_FILE` | none | Absolute path of the [per-workspace overrides](#per-workspace-overrides) file, read again on every sync. |
| `SANDBOX_POOL_SYNC_INTERVAL` | `30s` | How often the account's workspaces are listed again. A workspace joined is served from the next pass; one left out of two listings in a row drains like a removed one (`SANDBOX_POOL_DRAIN_TIMEOUT`), and one that comes back while draining is served again with its leases intact. A failed or empty listing changes nothing. Each pass also retries sharing runtimes whose visibility change failed, and re-sends pods the workspaces-changed hint (at most every 30s) while a workspace has no upstream registration. |
| `SANDBOX_POOL_RECONCILE_INTERVAL` | `30s` | Lease reconciliation period; `0` turns it off (and with it the retry of failure reports). See [Lease end and reconciliation](#lease-end-and-reconciliation). |
| `SANDBOX_POOL_LEASE_MAX` | `2h` | Longest a lease may last. A task still leased after it is failed upstream ("task exceeded the maximum lease time") and its pod reset. |
| `SANDBOX_POOL_RUNTIME_VISIBILITY` | `public` | Visibility set on the runtime registered in each workspace. A runtime starts private to its owner, so `public` is what lets every member bind agents to it; `private` leaves sharing to the owner. |
| `MULTICA_SERVER_URL` | none | Multica server base URL. |
| `SANDBOX_MANAGER_LISTEN` | `:8790` | Pod API listener. Also serves `/healthz` (liveness: always 200) and `/readyz` (200 only if a workspace listing succeeded within three sync intervals, else 503). |
| `SANDBOX_POOL_ADMIN_LISTEN` | `127.0.0.1:8791` | Admin listener (inventory, leases). |
| `SANDBOX_MANAGER_NAME` | `Managed Sandbox` | Machine name: the title of the manager's entry in Multica's runtime list. Each runtime is named `<agent CLI> (<name>)`, as the stock daemon names its runtimes after its host, which is where the list reads the title from. No parentheses or ` · `. A rename in the UI wins over it. |
| `SANDBOX_MANAGER_DEVICE_NAME` | `SANDBOX_MANAGER_NAME` | The line under the title (the registration's `device_name`). No parentheses or ` · `. Both names reach Multica when a workspace registers, so after a manager restart. |
| `SANDBOX_MANAGER_DAEMON_ID` | derived | Daemon ID the manager uses in every workspace. Unset, it is derived from `MULTICA_SERVER_URL` and the bot account, the same on every restart. Changing it registers new runtimes. |
| `SANDBOX_POOL_SIZE` | 0 | Declared pod count, shown next to live counts. |
| `SANDBOX_PROVIDERS` | all | Comma list of agent CLIs exposed as runtimes. Set it to the image's CLIs: a pod can otherwise add runtimes of any type. |
| `SANDBOX_MODELS` | none | Models the agent settings picker offers, per agent CLI: `claude=claude-sonnet-4-6,claude-opus-4-7;codex=gpt-5.2`, the first of each list the default. The pods run one image, so the manager answers the picker for every workspace from a pod's latest report, for ten minutes, and asks a pod again after that; these lists answer it until a pod has reported, or when no pod reports. Unset, the picker waits for a pod. |
| `SANDBOX_POOL_DRAIN_TIMEOUT` | `10m` | How long a removed workspace keeps its leases. |
| `SANDBOX_POOL_HEARTBEAT_TIMEOUT` | `45s` | Sandbox service. Silence after which a pod is listed lost. |
| `SANDBOX_POOL_RESET_TIMEOUT` | `2m` | Sandbox service. Longest a pod may stay resetting; past it the pod is lost. |
| `SANDBOX_POOL_LOST_RETENTION` | `10m` | Sandbox service. How long a lost pod stays in the inventory before it is pruned (counted in `pruned_total`); from then on its pod token is refused. |

The manager handles no git or LLM credentials. Agents reach an LLM and a git
host with their own custom environment in Multica, which the daemon passes to
the agent unchanged.

## How a pod registers

Pods are identified by a pod token the manager issues (ADR 0003), never by
their address, so a mesh that rewrites source addresses changes nothing:

1. The pod's entrypoint runs once per container start. It calls
   `POST /pod/register` (`POD_REGISTER_URL`) with `{"label": "<hostname>"}`
   (`POD_LABEL` overrides it; in Kubernetes it is the pod name) and gets
   `{"pod_id": "...", "token": "mpt_..."}`. It tries every 2 seconds, up to
   `POD_REGISTER_ATTEMPTS` (60) times, then exits so the container restarts.
2. It writes the token into the daemon's `.multica/config.json`, in place of
   the image's placeholder, and starts the supervisor, which starts the daemon.
3. The daemon presents the token as its bearer token on every daemon call, and
   the supervisor reads it from the same file and presents it on
   `/pod/control`. The manager resolves it to the pod; a missing or unknown
   token gets `401 unknown pod token`.

The sandbox service assigns the pod ID; the label is only shown in the
inventory and logs. A new pod is reset once before it can be acquired. The
token lasts until the container stops: a restarted container registers as a
new pod, and the old identity, now silent, is marked lost after the heartbeat
timeout and the manager ends its leases (tasks failed upstream). The
manager never sees the token: it asks the service which pod a token
belongs to. An agent's `multica` CLI calls carry the
task's `mat_` token instead and are relayed to Multica unchanged, without the
manager knowing which pod sent them.

The registration endpoint is unauthenticated: anything that can reach the
manager's pod port can register as a pod and receive tasks, with each agent's
custom environment. Only the NetworkPolicies (or the compose network) keep
other callers away from that port.

## How a pod is reset and rejoins

The service needs nothing from the daemon to tell a reset has finished. A pod
is `free`, `held` (acquired by the manager), `resetting` or `lost`:

1. A reset starts when the manager hands a pod back with `Delete` (its last
   lease ended, a hold ran out, a task of another key evicted it, and so on),
   when a pod registers for the first time, when a lost pod returns, or when
   the supervisor reports an interrupt. The pod is `resetting` and the
   supervisor, polling `/pod/control`, is told to reset. `Delete` returns at
   once.
2. The supervisor deletes everything under the daemon's HOME except the skip
   list (`POD_DAEMON_HOME_SKIP`, default `.multica,.claude/skills`: the daemon
   config and the image's Claude skills the entrypoint writes at container
   start; entries are paths relative to HOME and may be nested), then makes the daemon's empty TMPDIR
   (`HOME/tmp`) again. Every per-task location (task directories, TMPDIR, the
   agents' HOME and caches) is under that HOME, so nothing a task wrote is
   left. It carries on past anything it cannot delete, stops no process,
   restores no template and touches nothing outside the daemon's HOME. It then
   reports `done` with the paths it could not delete.
3. The report completes the reset: the service logs any paths left behind and
   the pod is `free` again, so the manager can acquire it. A pod is never
   lent while it is resetting. The daemon keeps running throughout, so a
   registration during the reset is the same daemon's and changes nothing.
   A reset not reported done within `SANDBOX_POOL_RESET_TIMEOUT` makes the pod
   `lost`; a late report from it never frees it.

A supervisor whose control URL answers 401 (the service refuses the pod's
token: the manager restarted, or it pruned the pod) exits non-zero at once, so
the container restarts and registers anew. One that keeps answering 404 does
so after 5 minutes.

The supervisor also reports two events on `/pod/control`: `started` when it
(so the container) starts, before the daemon, and `daemon-exited` when the
daemon exits by itself, before the daemon is started again. Either one on a
pod the manager holds is an **interrupt**: the service releases the hold,
starts the reset itself and notifies the manager, which fails the pod's
started tasks upstream ("the pod's daemon restarted"), ends their leases with
"pod daemon restarted" or "pod container restarted", and places the tasks not
yet started on another pod. Without this a daemon the supervisor restarted,
which presents the same pod token, would register again as the same pod and
inherit leases whose tasks died with the old process.

The supervisor's start clears the daemon's HOME the same way (a restarted
container keeps its volume); see [SANDBOX.md](SANDBOX.md).

A pod that stays silent past the heartbeat timeout is `lost`. If the manager
holds it, the service notifies it and the manager ends the pod's leases
(tasks failed upstream, release audited) and places the tasks not yet started
elsewhere; the manager also compares the pods it holds with the service's
inventory on every tick, so a missed notification is caught. Any poll or
control report from a lost pod starts a new reset, so it returns to service
through a reset, whether or not it held work. The manager does not answer a
lost pod's poll with an error, and a pod token the service does not know gets
`401`.

After a manager restart, whose service state is in memory, every pod's token
is unknown: each supervisor exits at its next control poll (every 2 seconds),
the container restarts and registers as a new pod, and the service resets it
before it can be acquired. The manager's first workspace listing, before any
pod is served, fails what Multica still shows running on the manager's runtimes
in every workspace (orphan recovery), so the tasks that ran at the restart are
failed upstream without waiting for a pod. A pod's first registration in a
workspace recovers that workspace's runtimes again, which covers a workspace
joined later or a runtime registered anew. The `404 runtime not found` answer
goes to a pod heartbeating a runtime that Multica reported gone (the manager's
own upstream heartbeat got `404 runtime not found`: deleted in the UI, or
collected after a long time offline): the manager drops that workspace's
registration, tells the pods to read their workspace list again, and the
pod's registration that follows registers the workspace upstream again.

The manager heartbeats its registered runtimes upstream itself, every 15
seconds, for as long as it runs. The pods' heartbeats are answered by the
manager and never relayed. So a runtime shows online whenever the manager
runs, pods or not; with no pod free, nothing is claimed and tasks stay
queued in Multica.
Orphan recovery (`recover-orphans`) runs only for a workspace that holds no lease or claimed task
in the manager.

## Pod API authorization

Pods are identified by their pod token only. On top of the endpoint allowlist:

- Every daemon and control call needs a pod token the sandbox service issued;
  anything else gets 401. That includes the workspace list (`/api/workspaces`,
  `/api/daemon/workspaces`). Agent CLI calls with a `mat_` task token outside
  the daemon protocol are relayed to Multica as they are; a pod token there
  gets 403.
- A workspace's repos and settings (`/api/daemon/workspaces/{id}/repos`), which
  the manager reads with its PAT, are served only to a pod that holds a task of
  that workspace (leased or queued on it), or to a pod that runs no task and
  registered for the workspace (idle, or the new daemon of a reset about to
  complete). A pod working for another workspace gets 403; the daemon's
  settings refresh for its other workspaces then fails quietly.

## Lease end and reconciliation

A lease ends and the pod is reset
(a shared pod: once its last task ends) in these cases; `end_reason` in
`/v1/leases` says which:

| Cause | `end_reason` | Task failed upstream? |
| --- | --- | --- |
| The pod reports `complete`, `fail` or `cancel-ack` and Multica takes it | `pod reported <action>` | no (the pod reported it) |
| Multica refuses that report with 401, 403 or 404 | `upstream refused the pod's <action> report (HTTP <code>)` | no (Multica no longer knows the task) |
| Reconciliation finds the task completed, failed or cancelled | `upstream task ended: <status>` | no |
| Reconciliation gets 404 for the task | `upstream task ended: not found` | no |
| Reconciliation gets 401 or 403 | `upstream access lost (HTTP <code>)` | no (it cannot be) |
| The lease is older than `SANDBOX_POOL_LEASE_MAX` | `lease exceeded the maximum lease time` | yes: "task exceeded the maximum lease time" |
| The pod's daemon or container restarted (supervisor event) | `pod daemon restarted` / `pod container restarted` | yes: "the pod's daemon restarted" |
| The binding is past its maximum lifetime and drain grace | `maximum lifetime and drain grace passed` | yes: "the sandbox pod reached its maximum lifetime and the task ran past the drain grace period" |
| The pod was lost, or its workspace's drain timed out | `pod lost` / `workspace removed from the configuration` | yes |

Reconciliation runs every `SANDBOX_POOL_RECONCILE_INTERVAL`: it asks Multica
for each leased task's status with the endpoint the stock daemon polls
(`GET /api/daemon/tasks/{id}/status`, with the PAT), at most 8 calls at once
and 10s each, never holding the manager's lock while it waits. Any other error
keeps the lease for the next pass.

A failure report that does not reach Multica (network error, timeout, 429 or
5xx) is queued and sent again by every reconciliation pass until Multica takes
it, refuses it with another 4xx (taken as already handled), or 30 minutes have
passed. `/v1/inventory` shows the queue length as `pending_fail_reports`.

## Inventory and readiness

`/v1/inventory` also reports `last_sync` (`ok`, `at`, `error` of the latest
workspace listing), `oldest_lease_age_seconds` (a value near the maximum lease
time points at a task nobody reports on), `pending_fail_reports`, and for each
workspace `upstream` (`ok`, `op`, `at`, `error` of its latest claim or
heartbeat to Multica) and its fair share: `guarantee`, `guarantee_in_effect`
(scaled to the live pods), `weight`, `occupied_pods`, `holding_pods`, `queued`
and `target_pods`, the pods it would hold if the live pods (`live_pods`) were
split by the rules below for the work there is now.

`/v1/dispatch` puts those shares side by side: the pod counts of the pool,
what every pod is doing (its workspace, tasks and hold), and for each
workspace its guarantee, weight, quota, target pods and the pods it occupies
(running and holding), with totals. `/dispatch` on the admin listener is a
page that shows it and reads it again every 10 seconds: a grid of the pods
colored by workspace, and each workspace's pods against its target, the
furthest short first. It tells a workspace waiting for a pod (short, with
tasks queued) from one whose guarantee is only kept free (short, nothing
queued). The workspace-reloaded log line is INFO only when a
listing changed something.

`/readyz` turns 503 when no listing succeeded for three sync intervals: a
revoked or expired PAT, or Multica unreachable. `pool/k8s/manager.yaml` points
the readiness probe at it, and its Services set `publishNotReadyAddresses`, so
a not-ready manager is visible (`kubectl get pods`, alerts) while pods keep
reaching it; cutting them off would stop their heartbeats and end every lease.

If the manager itself stalls longer than the heartbeat timeout (CPU
throttling, a paused process), the pods get one grace period instead of all
being marked lost at once.

Reset events are audited (`action: reset`), with the task whose end caused
it when there was one. The service writes every reset's start, completion
and timeout (a `Delete` that starts no reset writes nothing), naming the
manager's lease for the resets the manager causes, and a pod lost on its
heartbeat. `/v1/leases`
and the pods in `/v1/inventory` name the initiators. Each pod in
`/v1/inventory` also shows its `binding`: the affinity key, capacity use and,
in hold, `hold_until` and `hold_remaining_seconds`.

## Placement and pod lifetime

Since ADR 0009 a pod runs one task at a time and never serves another
workspace. A task's key is its issue, else its chat session; a task with
neither has none. A **pool** task takes its key's held pod, then an idle pod
of its workspace, then a warm pod, then a new one; a **clean** task its key's
held pod, then a warm pod, then a new one. When a task ends its pod is held
for its key; with no key or no hold, a pool pod becomes idle and a clean pod
is deleted. When the budget is full, an idle pool pod goes first, then a held
pool pod, then a held clean pod, from the workspace furthest over its share,
never one within another workspace's guarantee. The affinity rule, pod
capacity, placement wait, maximum lifetime and drain grace below are still
read and checked, but no longer used; issue 07 removes them, and the
sections on bindings below describe the GitOps pool before ADR 0009.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_RUNTIMES` | `pool` | The runtime kinds registered in every workspace: `pool`, `clean` or `pool,clean`. Each kind is a machine of its own upstream: the pool kind under the manager's daemon_id, the clean kind under it followed by `-clean`. Removing a kind stops heartbeating its runtimes, so they show offline. |
| `SANDBOX_MANAGER_CLEAN_NAME` | `SANDBOX_MANAGER_NAME` followed by ` Clean` | The machine name of the clean runtimes. |
| `SANDBOX_MANAGER_CLEAN_DEVICE_NAME` | the clean name | Shown under it. |
| `SANDBOX_POD_MAX_IDLE` | `24h` | How long an idle pool pod is kept before it is deleted. |
| `SANDBOX_PLACEMENT` | `person-issue` | No longer used (ADR 0009). |
| `SANDBOX_POD_CAPACITY` | 20 | No longer used (ADR 0009). |
| `SANDBOX_PLACEMENT_WAIT` | `30s` | No longer used (ADR 0009). |
| `SANDBOX_POOL_WAKE_INTERVAL` | `1s` | Dispatch tick. `0` does not turn dispatch off: the tick then runs every second and only the wake socket hints are disabled. |
| `SANDBOX_HOLD_TIMEOUT` | `10m` | How long a pod whose task ended is held for the task's key. Then a pool pod becomes idle and a clean pod is deleted. `0` holds nothing. A hold is never a guarantee: it ends early when the budget is full. |
| `SANDBOX_POD_MAX_LIFETIME` | `2h` | No longer used (ADR 0009). |
| `SANDBOX_POD_DRAIN_GRACE` | `10m` | No longer used (ADR 0009). |

## Bindings, hold and eviction

A task's affinity key comes from the workspace's rule:
`<rule>:<unit>/by:<initiator>/in:<workspace>`, where the unit is the issue
(`issue:<id>`), the chat session (`chat:<id>`), the agent or `all`, and the
initiator is the person the task runs for (`attribution.originator.id` in
Multica's task). A task with no unit or no known initiator, and every task
under the `task` rule, gets a key of its own (`task:<id>`).

A **binding** is one affinity key on one pod. The task goes to the most
recently used binding of its key that has a free slot; with none, it opens a
new binding on the pod reserved for its claim. Nothing is cleared between the
tasks of one binding, so the next task finds the files, sessions and caches
the last one left. When no task of the binding runs, it is in **hold** for
`SANDBOX_HOLD_TIMEOUT`: a task of the same key that arrives meanwhile takes
the pod again, and the countdown starts afresh when that task ends. A hold
that runs out resets the pod (`hold expired`). A binding whose key is its
task's own resets as soon as the task ends, since nothing could renew it.

The `workspace` rule is the loosest. Its key is `tenant/in:<workspace>`: every
task of the workspace, whoever it runs for (or nobody), shares its bindings,
so different people's tasks see each other's files. Its bindings are
**kept**: a pod in hold stays bound to the workspace with no hold timeout and
no maximum lifetime (`kept` in the inventory), and the workspace's next task
goes to one of its kept pods with a free slot before any other pod is taken.
Only an eviction ends a kept binding, for another workspace's task when the
workspace occupies more than its `guarantee` (see fair sharing below), or the
workspace's removal, or the pod's loss. Give the workspace a guarantee to keep
that many pods for good.

A pod in hold never shrinks usable capacity. When no pod serving a workspace
is idle, the manager reserves the pod whose hold has been idle longest and
claims against it. A task of that hold's key renews it. A task matching
another binding with room goes there, and the hold stays. A task of any other
key evicts the hold: the pod is reset (`evicted for task <id> of another
affinity key`) and the task runs there once the reset is done. A binding with
a running task is never evicted; a task no pod can take stays queued in
Multica.

Eviction is carried out in the manager: the held pod is reserved and the
claim made, and when the claimed task has another key the hold ends and the
pod is handed back with `Delete`. The task then waits, within
`SANDBOX_PLACEMENT_WAIT`, and takes the next pod the manager acquires
before any new claim is made. It fails upstream if no clean pod comes in
time.

A workspace removed from the configuration keeps no pod: its bindings in hold
are reset at once, and its other bindings when their last task ends (`workspace
removed`), including the tasks the drain timeout ends.

## Per-workspace overrides

The affinity rule, hold timeout, maximum lifetime and quota above are pool
defaults, and every workspace has a guarantee of 0 and a weight of 1. `SANDBOX_WORKSPACE_OVERRIDES_FILE` names a file (an absolute path)
that overrides them for single workspaces, keyed by workspace slug. Pod
capacity (`SANDBOX_POD_CAPACITY`) and the drain grace stay pool-wide.

```yaml
# overrides.yaml: YAML or JSON, one entry per workspace slug.
acme:
  placement: person        # SANDBOX_PLACEMENT
  hold_timeout: 1h         # SANDBOX_HOLD_TIMEOUT; 0 resets at once
  max_lifetime: 4h         # SANDBOX_POD_MAX_LIFETIME; 0 means none
  quota: 6                 # SANDBOX_POOL_QUOTA; 0 is no cap
  guarantee: 2             # pods kept for it; 0 (the default) keeps none
  weight: 2                # its share of the pods beyond the guarantees; 1 by default
sandbox-lab:
  hold_timeout: 0
```

A field left out keeps the pool default. A workspace without an entry uses
the pool defaults.

The file is read again on every sync (`SANDBOX_POOL_SYNC_INTERVAL`) and on
SIGHUP, so an edit applies without a restart. A Kubernetes ConfigMap update
reaches the file within a minute or so. A changed setting applies to new
claims and placements only. Running tasks are never disturbed:

- a binding keeps the maximum lifetime it opened with;
- a binding in hold keeps its countdown;
- a quota lowered below the running tasks only stops new claims.

The workspace-reloaded log line lists the changed workspaces as `settings_updated`.

Problems never stop the manager or the other workspaces. Each one is logged
once, as a WARN `workspace override not applied` line, when the set of
problems changes, and is listed in `override_problems` in `/v1/inventory`
until it is fixed:

- an entry with an unknown field or a bad value is left out. The workspace
  keeps its previous override, or the pool defaults when it had none;
- a slug that no workspace of the account has is ignored;
- a file that cannot be read or parsed keeps every override in effect.

Each workspace in `/v1/inventory` shows its `slug` and the settings in effect:
`placement`, `hold_timeout`, `max_lifetime` and `quota`. Slugs come from
the server's member listing (`GET /api/workspaces`), which is read only while
the file has entries.

## Task dispatch

The stock daemon in a pod polls for a task every 30 seconds, so a task would
wait that long for an idle pod. The manager therefore dispatches itself: the
daemon opens its usual wake websocket on the manager, and every
`SANDBOX_POOL_WAKE_INTERVAL` the manager reads the queue, acquires a pod
from the sandbox service with `Create` when it has no reservable one, claims
one task upstream against that reservation, places it where the lending policy
says, and sends the target pod the hint that makes it claim at once. The
socket carries hints only: claims, heartbeats and results stay on HTTP.

Claims are made only on the tick. A pod's poll never claims upstream: it
receives only a task already placed on it, and a poll with nothing placed on
it causes no upstream claim and never fails because Multica does. So a pod
without a socket or with a lost hint is served by its next poll once the tick
has placed a task on it, and the poll remains the liveness signal. A pod the
manager acquired but never ran a task on, because a claim came back empty,
is kept for the next claim, not handed back. Neither Multica nor the pod image
changes.

### Fair sharing, guarantees and quota

The manager shares the pods between workspaces, not tasks: a pod serves one
workspace at a time, so a workspace **occupies** the pods its bindings hold,
running or in hold, and the pods reserved for its claims.

1. Every dispatch tick it reads the pending list of each of its runtimes in
   each served workspace (`GET /api/daemon/runtimes/{id}/tasks/pending`;
   Multica v0.6.4 has no workspace-wide list, so a workspace costs one read
   per agent CLI per tick) and keeps the `queued` tasks.
2. Among the workspaces with a queued task and room under their quota
   (`SANDBOX_POOL_QUOTA` or the workspace's `quota`, which counts the slots of
   its running tasks and claims in flight; a binding in hold counts nothing),
   it picks the one with the fewest pods occupied beyond its `guarantee`, per
   unit of `weight`; between equals, the one whose next task has waited
   longest. Priority orders tasks within one workspace only: one workspace's
   urgent tasks never jump another's.
3. A workspace's **guarantee** (pods, `0` by default) is kept for it: while it
   occupies fewer pods than its guarantee, other workspaces may not take the
   rest, even when it has nothing to run. Its own running and holding pods
   count toward it. The pods beyond the guarantees go by **weight** (`1` by
   default): with two busy workspaces of weights 2 and 1, they settle at two
   pods to one. A lone busy workspace may use every pod not kept by a
   guarantee. When the guarantees add up to more than the live pods (free,
   held or resetting; not lost), each is scaled down in proportion.
4. Running tasks are never stopped to rebalance. A workspace over its share
   gives pods back as its bindings end. When no pod is free for a workspace
   with a task queued (the pool has none, or the guarantees keep the rest for
   others), a hold is evicted for it: its own, or one of the workspace furthest
   over its share. A hold within its workspace's guarantee is never evicted. Rebalancing therefore takes at most
   the maximum lifetime plus the drain grace (2h10m by default).
5. For the workspace picked, it **reserves** a place before it claims: a
   free slot on a pod it holds or, when it holds none for that workspace, a pod
   acquired from the sandbox service (a pod whose daemon did not register for
   the workspace is never lent to it) or, when the service has none (no
   capacity), the hold idle longest (see eviction above). With no place,
   nothing is claimed. Then it claims one task
   from that workspace against the reservation. Multica may hand out another
   queued task of that workspace than the one seen; that one is placed. An
   empty claim releases the reservation.

So a claimed task always has somewhere to run at once, and a task no pod can
take stays queued in Multica, where members see it waiting. A workspace with
quota and a free pod also gets at most one claim per tick when its list shows
nothing queued, because Multica promotes due retries and reclaims stale
dispatched tasks only when someone claims.

## Audit

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_AUDIT_FILE` | off | JSON Lines audit log, appended (0600). `-` writes to stdout. |

## Multica server version

The manager needs Multica v0.6.4 or later and no migration newer than it. It
calls, with the bot PAT: `GET /api/me`, `GET /api/daemon/workspaces` (and
`GET /api/workspaces` for slugs while the overrides file has entries),
`POST /api/daemon/register`, `POST /api/daemon/heartbeat`,
`POST /api/daemon/runtimes/{id}/recover-orphans`,
`GET /api/daemon/runtimes/{id}/tasks/pending`, `POST /api/daemon/tasks/claim`,
`GET /api/daemon/tasks/{id}/status`, `POST /api/daemon/tasks/{id}/fail`,
`GET /api/runtimes` and `PATCH /api/runtimes/{id}`; it relays the pods'
task calls (`start`, `progress`, `session`, `complete`, ...) and the agents'
`mat_` calls. `pool/compose/smoke/scenarios.sh` runs the pool end to end
against the `ghcr.io/multica-ai/multica-backend:v0.6.4` image and fails on
any call the backend answers with an error.

## Known limits and operations

- **Restarting or upgrading the manager fails every running task.** The
  sandbox service's state is in memory: after a restart every pod is reset and the tasks
  Multica still shows running on the pool's runtimes are failed (Multica retries
  those eligible). Upgrade when the pool is idle (`/v1/inventory` shows no
  `leased` pod), or accept the retries.
- **Accepting an invitation opens the pool to that workspace.** The manager
  serves every workspace its account belongs to, and its runtimes are `public`
  by default, so every member of a workspace the bot joins can bind agents to
  the pool within one sync interval. There is no allowlist and no fair-share
  floor: set `SANDBOX_POOL_QUOTA` so one workspace cannot take the whole
  pool. A workspace that needs a pool of its own needs another bot account and
  another manager deployment.
- **One daemon identity per bot account.** The manager is one machine to
  Multica, with the same daemon ID in every workspace, derived from the Multica
  URL and the bot account. Two managers with the same account against the same
  Multica register the same runtimes unless `SANDBOX_MANAGER_DAEMON_ID` keeps
  them apart. Registration names the former per-workspace daemon IDs, so
  Multica merges those runtimes into the new ones and agents stay bound.
- **Reserve then claim.** The manager claims a task only against a reserved
  place (a free slot on an idle pod, or a hold it may evict), so a claimed task
  never waits in the manager. Tasks no pod can take stay queued in Multica.
- **Claim load and dispatch rate.** Every tick reads one pending list per
  runtime of every served workspace, and active dispatch claims for one
  workspace at a time, at most once per workspace per tick even when nothing
  is queued: with many workspaces and an idle pod that is many requests per
  second to Multica.
  Each tick hands out at most one task, so filling many idle pods after a burst
  takes about one second per task (plus each pod's own 30s poll).
- **The PAT is a single point of failure.** It acts in every served workspace
  (its blast radius is everything that account can do in each of them, not just
  the pool), and when it is revoked or expires every workspace stops at once.
  The manager then keeps running: new tasks stay queued in Multica, reports of
  running tasks are refused and their leases end, `/readyz` turns 503, and
  `last_sync` and the workspaces' `upstream` show the error. To rotate it,
  create the new PAT, replace the Secret file, and restart the manager (which
  fails the running tasks, see above), then revoke the old PAT.
- **The CNI must enforce NetworkPolicy.** Pod registration is open, so only
  the NetworkPolicies keep anything but pool pods away from the manager's pod
  port, where anything can register as a pod and receive tasks. Use a CNI that enforces them (Calico in the reference setup) and
  check that node-local traffic (hostNetwork pods, the node itself) cannot
  reach port 8080 of the manager.
