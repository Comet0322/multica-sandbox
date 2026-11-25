# Deployment guide

The manager runs Multica tasks on pods it creates through the Kubernetes API,
one task at a time per pod (see [ARCHITECTURE.md](ARCHITECTURE.md)). Toward
Multica it is one account's local daemon: it serves every workspace that
account belongs to, finds new ones by itself, and needs no workspace list
(SIGHUP looks again at once).

## One namespace: Multica, the manager and its pods

Everything runs in one namespace:

```
namespace
├── Multica: Postgres, the backend, the web app      (deploy/k8s/multica)
├── the manager: one Deployment and its Service        (deploy/k8s/manager.yaml)
└── the pods the manager creates, one task each        (from the pod template)

a Postgres database of the manager's own: its dispatch record
```

- The manager reaches the backend at its Service (`http://multica-backend:8080`).
- The pods reach the manager at its Service (`http://sandbox-manager:8080`),
  which the pod template names in `SANDBOX_MANAGER_URL`. They need no other
  address in the namespace.
- The manager reaches the Kubernetes API with rights on pods only.
- The manager reaches its own Postgres database, which may be anywhere it
  can connect to.

Run one manager per namespace and per database: two would act on the same
pods and the same dispatch record.

### Steps

1. **Multica.** Create the Secret `multica-backend` with `POSTGRES_PASSWORD`,
   `DATABASE_URL` (pointing at `multica-pg:5432`) and `JWT_SECRET` (for a test
   backend only, also `MULTICA_DEV_VERIFICATION_CODE`), and the ConfigMap
   `multica-backend` with the public URLs `FRONTEND_ORIGIN`, `MULTICA_APP_URL`
   and `MULTICA_PUBLIC_URL` (no `APP_ENV=development` in production). Then:

   ```bash
   kubectl apply -k deploy/k8s/multica -n <namespace>
   ```

   Add the Ingress or mesh routes the namespace uses for the backend and the
   web app.
2. **The bot account.** Sign in to Multica as a dedicated account, create a
   personal access token for it, and invite it to every workspace the manager
   is to serve. The PAT is the manager's only Multica credential.
3. **The images.** Build and push the manager and the pod image (the
   `agents` target) from the repository root:

   ```bash
   docker build -f deploy/Dockerfile.manager -t <registry>/multica-sandbox-manager:<tag> .
   docker build -f deploy/Dockerfile.pod --build-context multica=../multica -t <registry>/multica-sandbox-pod:<tag> .
   ```

   The pod image builds the stock daemon from the working tree of a Multica
   checkout: check it out at the commit the server is built from (v0.11.0).
4. **The pod template.** Copy `deploy/k8s/pod.yaml`, set its image, and size
   its requests and limits ([The pod template](#the-pod-template)).
5. **Permissions.** Give the manager its rights on pods
   ([Permissions](#permissions)).
6. **The database.** Create a Postgres database and a role that owns it,
   for the manager alone, and a payload key
   ([The dispatch record](#the-dispatch-record-and-a-manager-restart)).
7. **The manager.** Apply `deploy/k8s/manager.yaml` after setting its
   placeholders ([Running the manager](#running-the-manager)).
8. **Check it** with the dispatch page, then move departments one at a time
   with the [runbook](../deploy/RUNBOOK.md).

## The pod template

`SANDBOX_POD_TEMPLATE` is the path of one Pod in YAML, which every pod is
created from; `deploy/k8s/pod.yaml` is the example. The manager fills in only
the pod's name, its label `app.kubernetes.io/managed-by=sandbox-manager`,
restart policy (`Never`), owner (with `SANDBOX_POD_OWNER`), the pod token
(`SANDBOX_POD_TOKEN` in the daemon's environment) and, when the template sets
none, the hostname `sandbox`. Everything else is the operator's:

- **The container.** The daemon's container is named `daemon` (or is the only
  one) and runs `tini -- multica-pod-entrypoint-kube`, the image's default:
  it writes the daemon's config from the pod token and `SANDBOX_MANAGER_URL`,
  sets a private umask and the daemon's home on the pod's volume, and becomes
  the stock daemon. See [deploy/SANDBOX.md](../deploy/SANDBOX.md) for the
  image.
- **Requests and limits.** One task runs per pod, so the limit is the task's.
  A task over it fails with `the pod ran out of memory (limit <limit>)`, and
  only that task. Measured on the `agents` image with no model reachable: an
  idle pod uses about 10 MiB (90 MiB peak while the daemon probes the agent
  CLIs at start); a pod starting a Claude Code task peaks near 145 MiB;
  opencode starting a task about 1.2 GiB. A real task's peak depends on what
  it builds or runs; size the limit for the heaviest work, and the requests
  for the usual.
- **The liveness probe** runs `curl` inside the container against the
  daemon's loopback health endpoint (`127.0.0.1:19514/health`). A pod whose
  daemon stops answering is killed, not restarted; its task fails and the pod
  is deleted.
- **One task at a time.** The sandbox service sets
  `MULTICA_DAEMON_MAX_CONCURRENT_TASKS=1` on the daemon, replacing any value
  the template gives. The manager hands a task that has not started to its
  pod again on the pod's next claim (after a lost answer or a manager
  restart). A daemon with a free slot would claim while it prepares the task
  and run it twice; with one slot it claims only while it runs nothing.
- **The hostname.** Without `spec.hostname` it is the pod's name, which
  agents print into what reaches Multica. Keep `sandbox`.
- **Security.** The example passes Pod Security `restricted`: non-root,
  read-only root filesystem, no capabilities, no privilege escalation,
  `RuntimeDefault` seccomp, no service account token. The only writable path
  is the `emptyDir` at `/home/pod`. Check it against the namespace's own
  admission policies with `kubectl create --dry-run=server -f <template>`. If a
  mesh injects a sidecar, keep the daemon's container named `daemon` and allow
  its traffic to the manager.

The template is read once at start; changing it takes a manager restart, and
applies to pods created from then on.

## Permissions

The manager needs, in its namespace, exactly these verbs on `pods`: `create`,
`delete`, `get`, `list` and `watch`; and, only with
`SANDBOX_POD_OWNER`, `get` on that one Deployment (a Role rule with
`resourceNames`). It needs nothing else: no `patch`, Secrets, `pods/exec`,
`pods/log` or cluster-wide rights. What each pod serves is in the manager's
database, not on the pod.

The manager authenticates in one of two ways:

- **A service account** (no `KUBECONFIG`): `deploy/k8s/rbac.yaml` creates the
  ServiceAccount `sandbox-manager`, a Role with the five verbs above and its
  RoleBinding. Creating them takes RBAC rights the operator may not have; then
  ask the cluster's administrators to apply it, or check it with
  `kubectl apply --dry-run=server`.

  ```bash
  kubectl apply -f deploy/k8s/rbac.yaml -n <namespace>
  ```

- **A kubeconfig** (`KUBECONFIG`), such as an operator's personal one, mounted
  from a Secret. Check what it may do first:

  ```bash
  for v in create delete get list watch; do kubectl auth can-i $v pods -n <namespace>; done
  kubectl auth can-i get deployments/sandbox-manager -n <namespace>   # only with SANDBOX_POD_OWNER
  kubectl config view --minify    # how it authenticates
  ```

  A kubeconfig whose credentials come from an `exec` plugin may hold a token
  that expires; the manager then stops creating pods until it is replaced.

When Kubernetes refuses the manager's credentials, no pod can be created and
nothing is claimed: tasks stay queued in Multica, and the error shows on the
dispatch page and in the log.

## Running the manager

`deploy/k8s/manager.yaml` is an example Deployment and Service:

- one replica, strategy `Recreate`;
- the service account from `rbac.yaml`, or a kubeconfig mounted from a Secret
  (commented out in the file);
- the PAT, the database's URL and the payload key from the Secret
  `sandbox-manager` (`SANDBOX_POOL_TOKEN_FILE`, `SANDBOX_DATABASE_URL_FILE`,
  `SANDBOX_PAYLOAD_KEY_FILE`), and the pod template and overrides from the
  ConfigMap `sandbox-manager`;
- liveness on `/healthz` and readiness on `/readyz` of the pod listener, with
  the Service publishing not-ready addresses, so pods can report their running
  tasks while Multica is unreachable;
- the admin API on loopback; reach it with
  `kubectl port-forward deploy/sandbox-manager 8791` and open
  `http://127.0.0.1:8791/dispatch`.

```bash
openssl rand -base64 32 >payload-key   # keep it with the database's backups
kubectl create secret generic sandbox-manager -n <namespace> --from-file=pool-token=<PAT file> \
  --from-file=database-url=<file holding the database URL> --from-file=payload-key=payload-key
kubectl create configmap sandbox-manager -n <namespace> \
  --from-file=pod.yaml=<your pod template> --from-file=overrides.yaml=<your overrides file>
kubectl apply -f deploy/k8s/manager.yaml -n <namespace>
```

The manager's own pod must not carry the label
`app.kubernetes.io/managed-by=sandbox-manager`, which marks its pods.

The manager can also run outside the cluster (as `deploy/k8s/dev/manager.sh`
does) with `KUBECONFIG` set, as long as the pods can reach its pod listener at
the template's `SANDBOX_MANAGER_URL`.

The settings are read once at startup; a bad value stops the process with a
message naming the variable. The exception is the
[per-workspace overrides](#per-workspace-overrides) file, which is read on
every sync and whose problems are reported without stopping anything.
Durations use Go syntax (`45s`, `10m`, `2h`). Unset or `0` means the default
shown, unless the table says otherwise.

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
| `SANDBOX_MODELS` | none | Models the agent settings picker offers, per agent CLI: `claude=claude-sonnet-4-6,claude-opus-4-7;codex=gpt-5.2`, the first of each list the default. For a listed CLI the manager answers the picker with that list at once, and never asks a pod. A CLI not listed is answered from a pod's report, which the manager reuses for ten minutes. Multica keeps the last answer up to a day and asks again when it is a minute old, so the first picker opened after a change can still show the old list. List only models every member can use: the picker is per runtime, not per member, and a model a member's key cannot use fails only when the task runs. Set it whenever the model credential is chosen per task (for example an LLM gateway sidecar that looks up the member's key by the task ID): a pod asked for models has no task, so its report is the CLI's own catalog or its built-in fallback, never what the members can use. |
| `SANDBOX_POOL_SYNC_INTERVAL` | `30s` | How often the account's workspaces are listed again. A workspace joined is served from the next pass; one left out of two listings in a row drains like a removed one, and one that comes back while draining is served again with its leases intact. A failed or empty listing changes nothing. |

### Dispatch record

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_DATABASE_URL` | none | Postgres URL of the manager's own database (secret; `_FILE` form accepted), for example `postgres://sandbox_manager:<password>@<host>:5432/sandbox_manager`. Required. See [The dispatch record](#the-dispatch-record-and-a-manager-restart). |
| `SANDBOX_PAYLOAD_KEY_FILE` | none | Absolute path of the key task payloads are encrypted with in the database: 32 bytes in base64 (`openssl rand -base64 32`). Required. Keep it with the database: the payloads of tasks not started yet do not decrypt without it, and the manager does not start. |

### Pods

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_POD_TEMPLATE` | none | Absolute path of the pod YAML every pod is created from (see [The pod template](#the-pod-template) and `deploy/k8s/pod.yaml`). Required. One Pod, with the daemon's container named `daemon` or as its only container; unknown fields are refused. |
| `SANDBOX_NAMESPACE` | the kubeconfig context's, else the pod's own | Namespace the pods are created in. |
| `KUBECONFIG` | none | Kubeconfig to reach Kubernetes with (for now, the operator's own). Unset, the manager uses its in-cluster service account. |
| `SANDBOX_RUNTIMES` | `pool` | The runtime kinds registered in every workspace: `pool`, `clean` or `pool,clean` (see [Runtime kinds](#runtime-kinds)). Removing a kind stops heartbeating its runtimes, so they show offline. |
| `SANDBOX_MAX_PODS` | `10` | The budget: the most pods at once, warm, starting, running, held and idle. |
| `SANDBOX_WARM_PODS` | `2` | Ready pods kept that have served no one. At least `1`, since the manager learns each workspace's runtimes from a pod's daemon (`0` is refused), and at most `SANDBOX_MAX_PODS`. |
| `SANDBOX_POD_START_TIMEOUT` | `3m` | How long a new pod may take until its daemon calls the manager. Past it the pod is deleted, and pod creation backs off (10s, doubling up to 5m). Nothing was claimed for it, so no task fails. |
| `SANDBOX_POD_OWNER` | none | `deployment/<name>`: the Deployment in the namespace that owns every pod created from then on (see [Pod owner and Argo CD](#pod-owner-and-argo-cd)), normally the manager's own. Needs `get` on that Deployment; the manager does not start when it cannot read it. Unset, pods have no owner. |
| `SANDBOX_POD_START_FAILURES` | `3` | How many pods in a row may fail to start for a passing reason (no node, the start timeout) before the pods count as [broken](#when-no-pod-starts). A failure that recurs on every pod (an image that cannot be pulled, refused credentials, a rejected template) breaks them at once. |
| `SANDBOX_HOLD_TIMEOUT` | `10m` | How long a pod whose task ended is held for the task's key. `0` holds nothing. Overridable per workspace. |
| `SANDBOX_POD_MAX_IDLE` | `24h` | How long an idle pool runtime pod is kept before it is deleted. |
| `SANDBOX_POOL_QUOTA` | `0` | The most pods one workspace's running tasks may occupy at once; `0` is no cap. Overridable per workspace. |
| `SANDBOX_WORKSPACE_OVERRIDES_FILE` | none | Absolute path of the [per-workspace overrides](#per-workspace-overrides) file, read again on every sync. |

### Listeners, dispatch and audit

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_MANAGER_LISTEN` | `:8790` | Pod API listener. Also serves `/healthz` (liveness: always 200) and `/readyz` (200 only if a workspace listing succeeded within three sync intervals, the dispatch record can be written and pods start, else 503). |
| `SANDBOX_POOL_ADMIN_LISTEN` | `127.0.0.1:8791` | Admin listener (see [Admin API](#admin-api-and-dispatch-page)). Not for pods. |
| `SANDBOX_POOL_WAKE_INTERVAL` | `1s` | Dispatch tick. `0` does not turn dispatch off: the tick then runs every second and only the wake socket hints are disabled. |
| `SANDBOX_POOL_RECONCILE_INTERVAL` | `30s` | Lease reconciliation period; `0` turns it off, and with it the retry of failure reports, which a restart then sends. See [Lease end and reconciliation](#lease-end-and-reconciliation). |
| `SANDBOX_AUDIT_FILE` | off | JSON Lines audit log, appended (0600). `-` writes to stdout. |

The manager handles no repository or LLM credentials. Agents reach an LLM and
a git host with their own custom environment in Multica, which the daemon
passes to the agent unchanged.

### Removed settings and endpoints

These belonged to the pool of pods that were started outside the manager and
reset between tasks, before pods were created through the Kubernetes API.
Each one still set is ignored, with a warning at start
(`<NAME> is no longer used and is ignored`):

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
  department, else a warm pod, else a new pod. Pool runtime pods are shared within a
  department: once a pod's hold ended, a task of another key, even another
  person's, may land on it and sees what earlier tasks left there.
- **Clean runtime.** A task takes its key's held pod, else a warm pod, else a
  new pod. A clean pod never serves another key.

When a task ends, its pod is held for the task's key for the hold timeout;
without a key, a pool runtime pod becomes idle at once and a clean pod is deleted.
When a hold ends, a pool runtime pod becomes idle and a clean pod is deleted. An idle
pool runtime pod is deleted after `SANDBOX_POD_MAX_IDLE`.

A **hold** is kept as long as possible but never guaranteed: when the budget is
full and a pod is needed, the manager deletes an idle pool runtime pod first, then a
held pool runtime pod, then a held clean pod, from the department furthest over its
share, and never one within another department's guarantee. A handoff across
pods (agent A hands an issue to agent B, and B lands on another pod) sees only
what A pushed, through a git MCP server for example; anything A left
unpushed stays on A's pod.

A **warm pod** is new and ready and belongs to no department or kind until its
first task. Before each claim the manager makes sure a ready pod exists (a
warm pod, or an idle pod of the department for a pool task), so a claimed task
always has somewhere to run; a task no pod can take stays queued in Multica.

## Pods and their failures

The manager acts only on pods labelled
`app.kubernetes.io/managed-by=sandbox-manager`. It fills in each pod's name,
that label, restart policy (`Never`), owner, pod token and, when the
template sets none, the hostname `sandbox`. A pod is ready once its daemon
first calls the manager with its token.

Every pod failure is classified: its **phase** (`create`, `start` or `run`),
its **kind**, and whether it is **permanent**, that is, recurs on every pod
until an operator acts. Each says what happened and what to do next, in
English. The latest 20 and the counts by kind since the manager started show
on the dispatch page and in the inventory (`failures`, `failure_counts`).

A ready pod that cannot run any more fails its task, which Multica shows as
`the pod running this task was lost: <what happened>. <what to do>`, and the
pod is deleted:

| Kind | Cause | What happened | What to do |
| --- | --- | --- | --- |
| `oom` | The pod's container ran out of memory | `the pod ran out of memory (limit <limit>)` | Split the task into smaller steps, or ask an administrator to raise the pod's memory limit |
| `evicted` | The node evicted the pod | `the node evicted the pod: <message>` | The node was short of resources; run the task again |
| `unresponsive` | The daemon's liveness probe failed and the container was killed (exit code 137, not out of memory) | `the pod's daemon stopped answering (its liveness probe failed)` | Run the task again; if it keeps happening, tell an administrator |
| `exited` | A container exited | `the pod's <container> container exited with code <code> (<reason>)` | Run the task again; if it keeps happening, tell an administrator |
| `deleted` | The pod was deleted outside the manager | `the pod was deleted` | Run the task again |

The pod's container is killed whole when it runs out of memory: on cgroup v2,
Kubernetes 1.28 and later kill every process in a container that hits its
limit (`memory.oom.group`), so the agent CLI cannot be made to die alone.

A pod that could not be created or did not start fails no task, since nothing
was claimed for it. It is deleted and pod creation backs off (10s, doubling up
to 5m):

| Kind | Cause | Permanent | What to do |
| --- | --- | --- | --- |
| `image` | The image cannot be pulled (`ErrImagePull`, `ImagePullBackOff`, `ErrImageNeverPull`, `InvalidImageName`); the pod is deleted as soon as this shows | yes | Check the image in the pod template and that the nodes can pull it |
| `invalid-template` | Kubernetes rejects the pod, or its container cannot be created (`CreateContainerConfigError`) | yes | Fix the pod template and restart the manager |
| `refused` | Kubernetes refuses the manager's credentials | yes | Check the manager's rights on pods |
| `unschedulable` | No node took the pod within the start timeout | no | Free capacity on the nodes, or lower `SANDBOX_MAX_PODS` or the pod's requests |
| `start-timeout` | The pod did not get ready within the start timeout for another reason | no | Check the pod's events and logs |
| `api` | Another failed Kubernetes API call | no | It is retried |

### When no pod starts

The pods are **broken** once one permanent failure happens, or
`SANDBOX_POD_START_FAILURES` (3) pods in a row fail to start. While broken:

- one pod at a time tries to start, within the backoff, to find out when pods
  start again; the budget is not filled with pods that cannot start;
- ready pods that already exist still serve; a task with none stays queued in
  Multica and fails nothing;
- the dispatch page and the inventory show `broken` and why, and `/readyz`
  turns 503 with `pods do not start: <why>`.

The first pod that gets ready clears it, with no restart: load the missing
image, or free the nodes, and the next try succeeds. A rejected template
needs the template fixed and a manager restart, since the template is read
at start.

What each pod serves is in the manager's database, and a restarted manager
takes over from it: see
[The dispatch record and a manager restart](#the-dispatch-record-and-a-manager-restart).

## Pod owner and Argo CD

With `SANDBOX_POD_OWNER=deployment/sandbox-manager` (set in
`deploy/k8s/manager.yaml`), every pod the manager creates carries an owner
reference to the manager's Deployment, read once at start:

- `kubectl` and Argo CD show the pods under the Deployment, with their
  health and events.
- Deleting the Deployment has Kubernetes delete every pod it owns, running
  tasks included; no pod is left behind once the manager is gone.
- The reference neither blocks the Deployment's deletion nor makes it the
  pods' controller, so it needs no right beyond `get` on the Deployment.
- Pods a previous run created without an owner keep running without one,
  until their hold or idle time ends.

Owning the pods by the manager's own pod or its ReplicaSet would not work:
both are replaced on every restart or rollout, and Kubernetes would delete
the pods with them.

**With Argo CD**, deploy the manager's Deployment, Service, ConfigMap and
Role from Git as usual; the pods it creates are not in Git and Argo CD does
not manage them:

- The Application stays Synced while pods come and go, and auto-sync with
  prune does not delete them: Argo CD prunes only resources carrying its
  tracking label or annotation, never their owned children. Keep Argo CD's
  tracking label (`app.kubernetes.io/instance` by default) and
  `argocd.argoproj.io/` annotations out of the pod template, which the
  manager copies onto every pod.
- The pods show in the Application's resource tree under the Deployment.
- When no pod starts ([broken](#when-no-pod-starts)) or Multica cannot be
  reached, `/readyz` fails, the manager's Deployment is not available and
  the Application turns Progressing, then Degraded past the Deployment's
  progress deadline.
- The template is read at start: changing the ConfigMap takes a manager
  restart, for instance by changing an annotation on the Deployment's pod
  template in the same commit.

## The dispatch record and a manager restart

The manager keeps its dispatch record in a Postgres database of its own
(`SANDBOX_DATABASE_URL`): the pods it holds, with what each serves, and every
task it claimed, from claim to end. It creates and upgrades its tables at
start, and keeps the schema's version in the table `schema_version`. Give it
an empty database and a role that owns it. It needs no other database right.
The table `pods` has one row per pod the manager holds or held: its phase
(`warm`, `running`, `held` or `idle`), workspace, kind, key, when its hold
ends or its idle time began, and when and why it went. The table `tasks` has
one row per task: its workspace, kind, key, pod, state and timestamps, why it
ended, and whether its failure report is still to be sent.

A task in the record is in one of four states:
- `placed`: claimed, and placed on a pod or waiting for one;
- `handed`: its pod's daemon was given it and has not reported it started;
- `running`: the daemon's start report reached Multica;
- `ended`: over, whatever the outcome.

A task's payload is the task as Multica handed it out, with the agent's
custom environment, which may hold credentials. The database keeps it
encrypted with AES-256-GCM, with the key in `SANDBOX_PAYLOAD_KEY_FILE`, and
only while the task is `placed` or `handed`. The manager clears it once the
task runs. Nothing about who started a task is in the database. Ended tasks
and pods that went are deleted 30 days later, unless a task's failure report
is still to be sent.

The manager writes the record as it changes, one write at a time and in
the background, so a slow database holds up no heartbeat, report, inventory
or Kubernetes event. Three steps wait for their own change to be written
before the manager acts on it:
- a claim waits until everything before it is written, so nothing is
  claimed while the record cannot be written;
- a pod gets a task only once the record says it was handed it, so a
  claimed task is in the record before a pod can get it; until then the
  task is not the pod's, and a pod lost meanwhile takes nothing with it;
- a failure is reported only once the record holds it pending, then again
  on every reconciliation pass until Multica takes it. A removed workspace is
  disconnected only once its failure reports are taken.

The rest (a task's start and end, holds, idle pods, pods let go) is written
a moment later. A manager that stops writes what is left before it exits;
what a crash loses, reconciliation rebuilds from Multica.

While the database cannot be written, the manager claims nothing and hands
no pod a new task (a pod handed one before gets it again), `/readyz`
returns 503 with `the dispatch record cannot be written: <why>`, and the
inventory's `problem` says the same. Pods keep
reporting on their running tasks. Once the database takes writes again, the
manager carries on with no restart.

When the manager starts, it reads the record and takes over:
- it adopts the pods the record holds that still exist, with their pod
  tokens, and deletes every other pod labelled
  `app.kubernetes.io/managed-by=sandbox-manager`, warm ones too; new warm
  pods are made;
- a running task runs on, and its pod's daemon reports through the new
  manager as before (it registers again on its next heartbeat);
- a task `placed` on a pod, or `handed` to it and not started, is handed to
  that pod again at its next claim, and runs;
- a task placed on no pod waits for one, as long as a new pod may take to
  start;
- a hold resumes with its remaining time, and an idle pod ages on from when
  it became idle;
- a pod the record holds that Kubernetes no longer has is lost at the first
  dispatch tick, so its running task is failed, and a task placed on it goes
  to another pod;
- the failure reports Multica had not taken are sent again.

The manager never asks Multica to fail what Multica shows on its runtimes. A
task it claimed but had not recorded when it stopped is handed out again by
Multica to the manager's next claim after 90 seconds, or failed by Multica as
a retryable timeout after 300 seconds.

### Moving from a manager that kept the record on its pods

Earlier versions kept the record in pod labels and annotations. The new
version does not read them. To move:

1. Scale the old manager's Deployment to 0 once the dispatch page shows no
   running task. Tasks queued in Multica wait.
2. Delete its pods:
   `kubectl delete pods -l app.kubernetes.io/managed-by=sandbox-manager -n <namespace>`.
3. Create the database, the payload key and the Secret, and deploy the new
   version ([Running the manager](#running-the-manager)). The Role no longer
   needs `patch`.

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

A failure report is sent once the dispatch record holds it pending.
One that does not reach Multica (network error, timeout, 429 or 5xx) is sent
again by every reconciliation pass until Multica takes it or refuses it with
another 4xx (taken as already handled), with no time limit, and a restarted
manager sends it again. A pass sends them at most 8 at once, beside the
status calls, so reports Multica is slow to take hold up no lease. `/v1/inventory` shows how many are pending as
`pending_fail_reports`.

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
  pods dropped from the list after 10 minutes); `problem`, why the dispatch
  record cannot be written, else why pods could not be created or started
  lately; `broken`, why no pod starts at all
  ([When no pod starts](#when-no-pod-starts)); `failures`, the latest 20 pod
  failures, newest first, each with `at`, `pod`, `phase`, `kind`,
  `permanent`, `what` and `advice`; `failure_counts`, every failure since the
  manager started by kind; `live_pods`; `last_sync`;
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
  `running`, `held`, `idle`, `failed`, `problem`, `broken`, `failures`,
  `failure_counts`), each pod (`id`, `name`,
  `phase`, `kind`, `workspace_id`, `key`, `task_id`,
  `hold_remaining_seconds`, `initiators`, `reason`), each workspace's share
  and its tasks (queued, starting, running, recently ended), and totals.
- **`GET /dispatch`**: a page that shows `/v1/dispatch` and reads it again
  every 10 seconds: each pod's kind and phase, whether pods are broken, pod
  creation failures with their reason, each workspace's pods against its
  target, and the latest pod failures with what to do.

`/readyz` on the pod listener turns 503 when no listing succeeded for three
sync intervals (a revoked or expired PAT, or Multica unreachable), while the
[dispatch record](#the-dispatch-record-and-a-manager-restart) cannot be
written, or while the pods are [broken](#when-no-pod-starts). Point a
readiness probe at it, but keep publishing a not-ready manager to its pods
(`publishNotReadyAddresses`), or their running tasks could not report.

Pod deletions and leases are audited (`action: delete`, `lease`, `release`),
with the task that caused them.

## Multica server version

The manager needs Multica v0.11.0 and no migration newer than it. It
calls, with the bot PAT: `GET /api/me`, `GET /api/daemon/workspaces` (and
`GET /api/workspaces` for slugs while the overrides file has entries),
`POST /api/daemon/register`, `POST /api/daemon/heartbeat`,
`GET /api/daemon/runtimes/{id}/tasks/pending`, `POST /api/daemon/tasks/claim`,
`GET /api/daemon/tasks/{id}/status`, `POST /api/daemon/tasks/{id}/fail`,
`GET /api/runtimes` and `PATCH /api/runtimes/{id}`; it relays the pods' task
calls (`start`, `progress`, `session`, `complete`, ...) and the agents'
`mat_` calls. It answers the pods' `tasks/{id}/environments` and
`environment-configs` reports itself: Multica would link members to dev
servers on a pod's localhost, and offer controls for them that nothing carries
out.

Multica serves a runtime and its tasks only to the account that registered
it, so the PAT must stay with that account: another account's PAT reaches
neither the manager's runtimes nor the tasks running on them.

## Known limits and operations

- **A pod deleted while the manager is down.** The record still holds it, so
  the restarted manager finds it lost at its first dispatch tick: its running
  task is failed in Multica, which retries the task when it is eligible.
- **While the manager is down, nothing is dispatched or served.** Pods'
  reports wait: the daemon keeps its complete and fail reports and sends them
  again for hours, and retries its start report for a few seconds. Progress
  and transcript messages a daemon sends meanwhile are lost.
- **A restart longer than 90 seconds.** Multica hands a task that was
  dispatched and not started to the manager's next claim again. The manager
  takes the new delivery for the task it holds, so the task runs once.
- **The payload key cannot be rotated in place.** The payloads of tasks
  placed or handed when the key changes do not decrypt with the new key, and
  the manager does not start. Change it only while the dispatch page shows no
  starting task, and restart at once.
- **The database is the manager's alone.** Two managers on one database
  would take over each other's pods. Back it up with the payload key.
- **Memory is per pod.** The pod template's limits bound each task; a task
  over them fails with the out-of-memory reason, and only that task. The
  whole container is killed, the daemon too (see
  [Pods and their failures](#pods-and-their-failures)).
- **Pool runtime pods are shared within a department.** Work that must not be seen by
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
- **A runtime's concurrency limit covers all its pods.** Multica's
  per-runtime `max_concurrent_tasks` (unset by default) counts every task of
  the runtime. Set on one of the manager's runtimes, it limits all of that
  runtime's pods together, and claims beyond it come back empty without an
  error.
- **The PAT is a single point of failure.** It acts in every served workspace,
  and when it is revoked or expires every workspace stops at once: new tasks
  stay queued in Multica, reports of running tasks are refused and their
  leases end, `/readyz` turns 503, and `last_sync` and the workspaces'
  `upstream` show the error. To rotate it, create the new PAT for the same
  account ([Multica server version](#multica-server-version)), replace the
  Secret file, restart the manager (running tasks carry on, see above), then
  revoke the old PAT.
- **Restrict who reaches the manager's pod port.** A pod token is needed for
  every daemon call, but anything inside a pod holds its token; keep other
  callers away from the pod listener with the namespace's network policy.

## Local kind namespace

`deploy/k8s/dev` stands in for your namespace on a workstation:

```bash
deploy/k8s/dev/up.sh
```

This creates the kind cluster `sandbox-dev` and the namespace
`multica-sandbox`, and runs Multica v0.11.0 in it (backend at
http://127.0.0.1:18080, web app at http://127.0.0.1:13001; sign in with any
email and the code `up.sh` prints). It writes to `deploy/k8s/dev/.state/`,
which git ignores:
- `backend.env`: the database password, the JWT secret and the fixed sign-in
  code;
- `dev-user.kubeconfig`: a kubeconfig with only the manager's rights (the five
  verbs on pods in the namespace, and `get` on the Deployment
  `sandbox-e2e-owner`), standing in for an operator's own;
- `manager-db-url` and `payload-key`: the manager's database and its payload
  key (`deploy/k8s/dev/manager-db.sh`).

It also creates `sandbox-e2e-owner`, a Deployment with no replicas that
stands in for the manager's own Deployment as the pods' owner while the
manager runs on the host (`deploy/k8s/dev/owner.yaml`), and the manager's
database: a role and a database `sandbox_manager` on the namespace's
Postgres, which the host reaches at the kind node's address on node port
30432 (`deploy/k8s/dev/manager-db.yaml`). The role may create databases: the
end-to-end scenarios make one per run. `manager-db.sh` runs on its own too,
to add the database to a cluster made before it existed.

The namespace enforces Pod Security `baseline` and warns about `restricted`.
Running `up.sh` again keeps the database and secrets; `down.sh` deletes the
cluster and the state. `kind create cluster` makes `kind-sandbox-dev` the
current kubectl context; switch back with `kubectl config use-context`.

The pods need the fake-agent image, built and loaded once (and after a change
to `deploy/`):

```bash
docker build -f deploy/Dockerfile.pod --build-context multica=../multica --target fake -t multica-sandbox-pod:kube-fake .
kind load docker-image --name sandbox-dev multica-sandbox-pod:kube-fake
```

Then run the manager on the host against the namespace:

```bash
deploy/k8s/dev/manager.sh
```

It signs in the bot `pool-bot@dev.test` with a workspace `dev`, creates its
pods from `deploy/k8s/dev/pod.yaml` with `dev-user.kubeconfig`, keeps its
dispatch record in `sandbox_manager`, and listens on
`:8792` for pods (reached at the kind network's gateway) and on
`127.0.0.1:8793` for the admin API (`/dispatch`).

The end-to-end scenarios run against the same namespace, or another one, with
`deploy/k8s/e2e/scenarios.sh` (see [its README](../deploy/k8s/e2e/README.md)).
`deploy/devpods/run.sh` runs the manager on simulated pods against the same
Multica, with load, to watch the dispatch page.

## Moving departments

Departments move one at a time, by rebinding their agents to the manager's
runtimes: see the [runbook](../deploy/RUNBOOK.md).
