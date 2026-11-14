# The pool on a Docker host (no Kubernetes)

`stack.yaml` (the manager) and `pool.yaml` (the pods) run the pool on one
Docker host, for example a VM. A pod is one container, built the way
`../k8s/sandbox/pool.yaml` builds it, with the same images.

```bash
cp sandbox.env.example sandbox.env        # fill in the values (see stack.yaml)
mkdir secrets                             # pool-token; mode 0440, group or owner 65532
mkdir overrides && cp overrides.yaml.example overrides/overrides.yaml  # per-workspace overrides; edit or empty it
docker compose --env-file sandbox.env -f stack.yaml -f pool.yaml up -d --scale pod=4
docker compose exec manager wget -qO- http://127.0.0.1:8791/v1/inventory
```

`smoke/smoke.sh` runs the whole stack once against a Multica backend (by
default the `v0.6.4` image) with its own Postgres, in its own compose project,
and tears it down: a pod registers through its entrypoint, runs a fake-agent
task, is reset and is lent again. See the script's header for the images it
needs.

`smoke/scenarios.sh` runs the end-to-end scenarios the same way, with one bot
account in two workspaces and two pods of one slot each, in about seven
minutes: registration through the entrypoint, one daemon identity, global
queue order, quota, reuse of a held pod and hold expiry, eviction, maximum
lifetime with the drain grace, and a reset that keeps the skip directories.
It rewrites the overrides file between scenarios to shorten the hold and
lifetime, and fails if the backend answers any call with an error.
`smoke/lib.sh` holds what both scripts share and lists their settings.

## What Kubernetes does for a pod that Docker does not

| Kubernetes gives the pod | On plain Docker | Here |
| --- | --- | --- |
| `fsGroup: 2000`: the volume is group 2000 and setgid | A new volume copies the image's ownership | The image's `/home/pod` is owned `1001:2000`, mode 2770 |
| Cluster DNS names baked into the image | Not resolvable | A network alias with the same name on the manager |
| `replicas` | `--scale` | Each replica has its own address and anonymous HOME volume |
| kubelet restarts an exited container | `restart: always` | Set on the pod |
| NetworkPolicy | None | See below |

## Network isolation

- Pods can reach the manager (8080) by name.
- The pods' network allows egress by default. Set `POD_NETWORK_INTERNAL=true`
  to cut pods off from everything else; the LLM endpoint and git host the
  agents' custom environment names, and the package mirror, must then be
  reachable on that network (attach them to `multica-sandbox`).
- Pods on one bridge can reach each other; there is no per-pod policy as on a
  cluster with NetworkPolicy. For tasks that must not meet, use one host or
  one network per trust group.
- A pod is identified by the pod token it gets when it registers, so the
  source address does not matter. Registration is open: anything on the
  `multica-sandbox` network can register as a pod, so attach nothing else to
  it.

## Operating

- Limits come from the `deploy.resources` values (including `pids`); Docker
  applies them without Swarm.
- A pod's HOME volume lives until `docker compose down -v`.
