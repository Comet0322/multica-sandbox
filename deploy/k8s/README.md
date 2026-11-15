# Kubernetes: Multica, the manager and its pods in one namespace

ADR 0009 runs everything in a single namespace:
- Multica (Postgres, the backend and the web app);
- the manager;
- the pods the manager creates.

| Directory | What it holds |
| --- | --- |
| `multica/` | Multica v0.6.4 with no namespace in the manifests. Apply it with `kubectl apply -k deploy/k8s/multica -n <namespace>` after creating the `multica-backend` Secret and ConfigMap (see below). |
| `dev/` | A local kind cluster that stands in for the company's namespace. |

The manager and the pod template are added here by `.scratch/k8s-executor` issues 02 and 03.

## Local namespace

```bash
deploy/k8s/dev/up.sh
```

This creates the kind cluster `sandbox-dev` and the namespace `multica-sandbox`, and runs Multica in it. It writes these files to `deploy/k8s/dev/.state/`, which git ignores:
- `backend.env`: the database password, JWT secret and the fixed sign-in code;
- `dev-user.kubeconfig`: a kubeconfig with only the rights known for certain at the company. It may create, delete, get, list and watch pods in the namespace, which is all the manager needs, and nothing else: no service accounts, Secrets, Deployments or RBAC. Multica itself is deployed with the kind cluster's admin credentials.

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
