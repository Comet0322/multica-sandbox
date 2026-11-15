# Managed sandbox runtime (sandbox manager)

Members see the manager's runtimes in each workspace: a pool runtime and, when
enabled, a clean runtime. Behind them, the manager creates pods through the
Kubernetes API, and each pod runs one task at a time with the stock `multica
daemon`. Agents therefore have the same features as on a local runtime.

```
Multica server ──/api/daemon/*── sandbox-manager ── Kubernetes API (create, watch, patch, delete pods)
                 (bot PAT)              ▲
                                        └── daemon and agent traffic ── pods (multica daemon + agent CLIs)
```

The manager creates every pod from the operator's pod template, with a pod
token in its environment, and knows it by that token. It reads the queued tasks
of every workspace, makes sure a ready pod exists, claims the next task by fair
share, priority and age, and places it: on its key's held pod, else (pool
runtime) an idle pod of its department, else a warm pod. After the task the pod
is held for the task's key until the hold timeout; then a pool pod is idle and
a clean pod is deleted. The manager handles no repository or LLM credentials:
agents bring their own in their custom environment in Multica.

Settings, the runtime kinds, holds, the budget, per-workspace overrides,
authorization, lease handling, the admin API and known limits are in
[POOL.md](POOL.md); the pod image is in [SANDBOX.md](SANDBOX.md); the pod
template, RBAC, restarts and the local kind namespace are in
[k8s/README.md](k8s/README.md).

| File | What |
|---|---|
| `Dockerfile.manager` | The manager |
| `Dockerfile.pool` | The pods (see [SANDBOX.md](SANDBOX.md)) |
| `k8s/` | The example pod template, the manager's RBAC, Multica's manifests and the local kind namespace |
| `devpool/` | Development only: the manager on simulated pods against a real Multica |
