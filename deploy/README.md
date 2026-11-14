# Managed sandbox runtime (sandbox manager)

Members see one runtime per workspace. Behind it, a fixed pool of pods runs the
agents' tasks, and every pod runs the stock `multica daemon`. A task borrows a
pod, runs, and the pod is reset before anyone else uses it. Agents therefore
have the same features as on a local runtime.

```
Multica server ──/api/daemon/*── sandbox-manager (pool manager) ── pods (multica daemon + agent CLIs)
                 (bot PAT)                ▲                              │
                                          └── daemon and agent traffic ──┘
```

Pods register themselves; the manager creates and deletes no sandboxes and does
not call the Kubernetes API, so the pool can sit on Kubernetes, Docker or VMs.
The manager handles no git or LLM credentials: agents reach both with their own
custom environment in Multica.

Each pod registers with the manager when its container starts and is known by
the pod token it gets. The manager reads the queued tasks of every workspace,
claims the next one by priority and age when a pod can take it and the
workspace is within its quota, and keeps a pod for the same person's next task
on the same issue for a while (a binding in hold) before it resets it.

Everything else (settings, the global queue, bindings, hold, eviction, pod
reset, per-workspace overrides, authorization, lease handling, security notes,
known limits) is in [POOL.md](POOL.md); the pod is in [SANDBOX.md](SANDBOX.md).
Manifests and the manual smoke test are in [pool/k8s](pool/k8s); the Docker
host setup and the end-to-end scenarios against a Multica backend are in
[pool/compose](pool/compose).

| File | What |
|---|---|
| `Dockerfile.manager` | The pool manager |
| `Dockerfile.pool` | The pods (see [SANDBOX.md](SANDBOX.md)) |
