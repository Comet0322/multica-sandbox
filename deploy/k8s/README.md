# Kubernetes: Multica, the manager and its pods in one namespace

ADR 0009 runs everything in a single namespace:
- Multica (Postgres, the backend and the web app);
- the manager;
- the pods the manager creates.

| Directory | What it holds |
| --- | --- |
| `multica/` | Multica v0.6.4 with no namespace in the manifests. Apply it with `kubectl apply -k deploy/k8s/multica -n <namespace>` after creating the `multica-backend` Secret and ConfigMap (see below). |
| `dev/` | A local kind cluster that stands in for the company's namespace. |
| `pod.yaml` | The example pod template (`SANDBOX_POD_TEMPLATE`): `tini` and the stock daemon, requests and limits, a liveness probe on the daemon's health endpoint, the fixed hostname `sandbox`, and no secret in the environment but the pod token the manager sets. It passes Pod Security `restricted`. Set its image and manager address. |
| `rbac.yaml` | The manager's own service account, with a Role that may only `create`, `delete`, `get`, `list`, `watch` and `patch` pods, and its RoleBinding: for when the manager stops using the operator's kubeconfig. |

A Deployment for the manager itself is not written yet.

## The pod

The image is the `agents` target of `deploy/Dockerfile.pool`. A pod the manager creates runs `tini -- multica-pod-entrypoint-kube` (`deploy/pool/entrypoint-kube.sh`), which writes the daemon's config from `SANDBOX_POD_TOKEN` and `SANDBOX_MANAGER_URL`, sets the private umask and the daemon's home on the pod's volume, and execs the stock daemon. The template's `securityContext` keeps the root filesystem read-only. A pod is never reused across departments; it is deleted after its task, its hold or its idle lifetime.

Without `spec.hostname` the pod's hostname is its name, which agents print into what reaches Multica. The template sets `sandbox`, and the manager sets the same when a template has none.

Memory measured on the `agents` image (cgroup `memory.current` and `memory.peak`, 2026-11-16), with no model reachable, so each task only started and retried its API calls:
- an idle pod (`tini` and the daemon): about 10 MiB, with a peak of 90 MiB while the daemon probes the agent CLIs at start;
- a pod running a Claude Code 2.1.325 task in the kind namespace: a peak of about 145 MiB (the CLI's RSS about 200 MiB);
- opencode 1.19.5 starting a task, in a plain container: a peak of about 1.2 GiB.

A real task's peak, with a model and the work it builds or runs, is not measured yet.

## What a pod serves, and a manager restart

The manager keeps no state of its own. It records on each pod what the pod serves, by patching the pod:
- labels `sandbox-manager/workspace` and `sandbox-manager/kind`;
- annotations `sandbox-manager/phase` (`running`, `held` or `idle`), `sandbox-manager/key`, `sandbox-manager/task`, `sandbox-manager/task-started`, `sandbox-manager/since` and `sandbox-manager/until`.

A warm pod carries none of them. When the manager starts it lists the pods labelled `app.kubernetes.io/managed-by=sandbox-manager` and takes them over:
- a running task runs on, and its pod's daemon reports through the new manager as before (it registers again on its next heartbeat);
- a hold resumes with its remaining time, and an idle pod ages on from when it became idle;
- a warm pod is kept and is ready again once its daemon calls in;
- a pod it cannot interpret (no pod token, a garbled record) is deleted;
- a task placed on a pod that had not taken it up, and a task Multica shows dispatched to the manager that no pod has, are failed, so Multica retries them.

Without the right to patch pods, nothing is recorded: the dispatch page shows the refusal, and a restarted manager deletes every pod it finds, failing the tasks that ran on them.

## The manager's own service account

```bash
kubectl apply -f deploy/k8s/rbac.yaml -n <namespace>
```

Creating a service account and a Role takes rights the operator may not have; then ask the cluster's administrators to apply it, or check it with `kubectl apply --dry-run=server`. Run the manager with `serviceAccountName: sandbox-manager`, `automountServiceAccountToken: true` and no `KUBECONFIG`.

## Local namespace

```bash
deploy/k8s/dev/up.sh
```

This creates the kind cluster `sandbox-dev` and the namespace `multica-sandbox`, and runs Multica in it. It writes these files to `deploy/k8s/dev/.state/`, which git ignores:
- `backend.env`: the database password, JWT secret and the fixed sign-in code;
- `dev-user.kubeconfig`: a kubeconfig with only the rights the manager needs. It may create, delete, get, list, watch and patch pods in the namespace, which is all the manager needs, and nothing else: no service accounts, Secrets, Deployments or RBAC. Multica itself is deployed with the kind cluster's admin credentials.

The manager uses `dev-user.kubeconfig`, as it will use the operator's own kubeconfig at the company.

- Backend: http://127.0.0.1:18080
- Web: http://127.0.0.1:13001 (sign in with any email and the code `up.sh` prints)

The namespace enforces the Pod Security `baseline` level and warns about `restricted`, since the company's policy is not known yet. Istio is not installed.

To run the manager against it, creating its pods in the namespace with `dev-user.kubeconfig`:

```bash
deploy/k8s/dev/manager.sh
```

This needs the pod image `multica-sandbox-pool:kube-fake`, built and loaded once:

```bash
docker build -f deploy/Dockerfile.pool --build-context multica=../multica --target fake -t multica-sandbox-pool:kube-fake .
```

```bash
kind load docker-image --name sandbox-dev multica-sandbox-pool:kube-fake
```

The manager runs on the host:
- pod listener `:8792`, which pods reach at the kind network's gateway;
- admin API `127.0.0.1:8793`;
- the bot `pool-bot@dev.test`, with its PAT kept in `.state`.

It uses the template `dev/pod.yaml`.

Running `up.sh` again keeps the database and secrets. `deploy/k8s/dev/down.sh` deletes the cluster and the state.

`kind create cluster` makes `kind-sandbox-dev` the current kubectl context. Switch back with `kubectl config use-context <name>` when needed.

## Moving to the company's namespace

1. **Create the Secret** `multica-backend` with:
   - `POSTGRES_PASSWORD`;
   - `DATABASE_URL`, pointing at `multica-pg:5432`;
   - `JWT_SECRET`.

   For a test backend only, also add `MULTICA_DEV_VERIFICATION_CODE`.
2. **Create the ConfigMap** `multica-backend` with the company's URLs: `FRONTEND_ORIGIN`, `MULTICA_APP_URL` and `MULTICA_PUBLIC_URL`. Leave out `APP_ENV=development` for production.
3. **Apply the manifests:** `kubectl apply -k deploy/k8s/multica -n <namespace>`. Add the Ingress or Istio routes the company uses for the backend and the web app.
4. **Point the manager at the namespace** with your kubeconfig.
