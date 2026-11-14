# Pool manifests and manual smoke test

Plain YAML for the sandbox manager (kustomize-friendly, no secrets).

| File | What |
|---|---|
| `kustomization.yaml` | Pool size (`replicas`) and image tags, version-controlled. Every `PLACEHOLDER` is replaced in an overlay |
| `namespace.yaml` | `multica-sandbox` namespace (Pod Security `restricted`: no pod runs as root or with a capability) |
| `manager.yaml` | Manager Deployment (1 replica) with its environment (Multica URL, audit to stdout) and a ClusterIP Service exposing the pod listener (`8080`) only. The admin listener (`8791`) is not in the Service |
| `networkpolicy.yaml` | Pod egress limited to manager (`8080`), DNS, the LLM endpoint and git host the agents' custom environment names, and the package mirror; the manager is matched by pod labels, external services by `ipBlock` placeholders. The manager's pod listener is reachable only from pool pods |

The pool image's manager address and control URL (`<manager>/pod/control`) are
baked in at build time (`--build-arg MANAGER_URL=http://sandbox-manager.multica-sandbox.svc.cluster.local:8080`).
The manager's env settings are described in `../../POOL.md`; the
admin listener binds to loopback inside the manager pod (reach it with
`kubectl port-forward`). One Secret is created out of band and is not in this
repo: `sandbox-manager-config` (key `pool-token`). The manager handles no git
or LLM credentials; agents bring them in their custom environment in Multica.
The manager image is `multica-sandbox-manager` (`../../Dockerfile.manager`);
the sandbox pod image and its Deployment are in `sandbox/`.

Offline check (no cluster): `kubectl kustomize deploy/pool/k8s`.

## Manual smoke checklist (real cluster)

Nothing here is automated. Record the results in ticket 20.
Set `NS=multica-sandbox`, `POD=$(kubectl -n $NS get pod -l multica.io/sandbox-role=pool-pod -o name | head -1)`,
`MGR=<manager pod-listener ClusterIP>:8080`.

1. **Deployed declaratively.** `kubectl apply -k` an overlay with real images, CIDRs and
   replicas. `kubectl -n $NS get deploy sandbox-pool` shows the replica count
   from git; `kubectl -n $NS get svc sandbox-manager` is `ClusterIP`, with port
   8080 only; `kubectl -n $NS get pod -o jsonpath='{..hostNetwork}'` prints nothing/false.
2. **Egress allowed** (from a pool pod, `kubectl -n $NS exec $POD -- ...`):
   - `curl -fsS http://$MGR/healthz`
   - `curl -fsSI <LLM endpoint URL>` and `curl -fsSI <git host URL>` (as the agents' custom environment names them)
   - `curl -fsSI <package mirror URL>/pypi/simple/`
3. **Egress denied** (each must time out or be refused; use `--max-time 5`):
   - `curl --max-time 5 https://example.com` (public internet)
   - `curl --max-time 5 http://<manager-ip>:8791/v1/inventory` (admin listener; must fail even if the policy were loosened, as it binds to loopback)
   - `curl --max-time 5 http://<another pool pod IP>:<any port>` (pod to pod)
4. **Manager only reachable from the pool.** From a throwaway pod WITHOUT the
   `multica.io/sandbox-role=pool-pod` label in the same namespace:
   `curl --max-time 5 http://$MGR/healthz` must fail.
5. **Every pod registered.** `GET /v1/inventory` (via
   `kubectl port-forward deploy/sandbox-manager 8791:8791`) lists one entry per
   pool pod, each with its own `id` and its Kubernetes pod name as `name`
   (`kubectl -n $NS get pod`). Pods are told apart by their pod token, so a
   mesh that rewrites source addresses changes nothing. Each pod passes
   through `resetting` once after it registers.
6. **Fake task: lease, release, reset.** Without LLM or git, run a fake task
   (the `fake` image target's agent) through the manager. Watch
   `curl -s localhost:8791/v1/inventory` and `/v1/leases`: pod goes idle ->
   leased (task id and initiator visible) -> in hold (`binding.hold`, for
   `SANDBOX_HOLD_TIMEOUT`; set it to `0` to skip) -> resetting -> idle. After
   the reset confirm in the pod: `kubectl -n $NS exec $POD -- ls -A /home/pod /home/pod/tmp /home/pod/.claude`
   shows only `.multica`, `.claude` (holding only `skills`) and an empty `tmp`.
7. **Inventory matches the cluster.** Idle + leased + resetting + lost equals
   the expected size, and the pod names in `/v1/inventory` equal
   `kubectl -n $NS get pod -l multica.io/sandbox-role=pool-pod`. Delete one
   pool pod: the replacement registers as a new pod, the old entry turns lost
   after the heartbeat timeout, and the count recovers.
8. **Custom environment reaches the agent.** Give the fake agent a custom
   environment variable `SANDBOX_FAKE_NOTE`; its comment on the issue ends
   with `note <value>`.

## Notes from the minikube smoke run (ticket 20)

- An overlay's `images:` entries must match the names the base kustomization
  already produced (`registry.example.com/multica/...`), not the short names in
  the Deployments.
- The manager reads its account PAT (`pool-token`) from a Secret volume and
  serves every workspace that account belongs to; add the account to a
  workspace and the pool serves it within `SANDBOX_POOL_SYNC_INTERVAL`.
- Lost pods stay in the inventory as `lost` for `SANDBOX_POOL_LOST_RETENTION`
  (default 10m) and are then pruned (`pruned_total` in the inventory counts
  them), so after pod replacement the count can exceed the declared size only
  until the retention runs out.
