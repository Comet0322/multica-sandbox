# Sandbox manager

Runs Multica tasks on pods it creates through the Kubernetes API, one task at a time per pod. Each pod runs Multica's stock daemon under `tini`; the manager plays the Multica server toward them and claims tasks upstream with a bot token.

| Path | What |
| --- | --- |
| `cmd/sandbox-manager` | The manager |
| `internal/manager` | Wires the parts together and serves the admin API |
| `internal/core` | The core: dispatch, the record of every task dispatched, leases, drains, reconciliation; knows tenants, opaque tasks and pod IDs only |
| `internal/multica` | What we know of Multica: its API, the daemon protocol served to the pods, runtime registration, a task's key |
| `internal/multica/tasksource` | The task source: the one daemon Multica sees; workspaces, runtimes, heartbeats, claims, overrides |
| `internal/multica/pool` | The pod side: the sandbox service plus the daemon protocol, wake sockets and the pods' daemon registrations |
| `internal/multica/devpool`, `cmd/sandbox-devpool` | Development only: a simulated pod side, to run the core and the task source against a real Multica without pods (`deploy/devpool/run.sh`) |
| `internal/multica/daemonwire` | The daemon protocol types we exchange with Multica, pinned to v0.6.4 |
| `internal/policy` | The policy: fair sharing, quota, placement by kind and key, holds, idle lifetime, budget deletion; knows no Kubernetes or Multica |
| `internal/sandbox` | The sandbox service: creates pods from the pod template, watches them, records what each serves on it, deletes them |
| `deploy/` | Dockerfiles, Kubernetes manifests and the local kind namespace, operator docs (`POOL.md`, `SANDBOX.md`, `k8s/README.md`) |
| `GLOSSARY.md` | Terms (decision records are kept outside the repository) |

Test with `go test -race ./...`. The pod image builds Multica's stock `multica` daemon from a Multica checkout next to this repository (`../multica`, at the version the company server runs, `slim/v0.6.4`). Build the images from the repository root:

```bash
docker build -f deploy/Dockerfile.manager -t multica-sandbox-manager:dev .
docker build -f deploy/Dockerfile.pool --build-context multica=../multica -t multica-sandbox-pool:dev .
```
