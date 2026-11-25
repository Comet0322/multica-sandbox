# Sandbox manager

Runs Multica tasks on pods it creates through the Kubernetes API, one task at a time per pod. Each pod runs Multica's stock daemon under `tini`; the manager plays the Multica server toward them and claims tasks upstream with a bot token.

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the components, the life of a task, pool and clean runtimes, fair sharing, warm pods and the budget, and the design trade-offs.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md): deploying into one namespace with Multica, the pod template, permissions, every setting, the local kind namespace; and [deploy/RUNBOOK.md](deploy/RUNBOOK.md) for moving departments.

| Path | What |
| --- | --- |
| `cmd/sandbox-manager` | The manager |
| `internal/manager` | Wires the parts together and serves the admin API |
| `internal/core` | The core: dispatch, the record of every task dispatched, leases, drains, reconciliation; knows tenants, opaque tasks and pod IDs only |
| `internal/multica` | What we know of Multica: its API, the daemon protocol served to the pods, runtime registration, a task's key |
| `internal/multica/tasksource` | The task source: the one daemon Multica sees; workspaces, runtimes, heartbeats, claims, overrides |
| `internal/multica/pool` | The pod side: the sandbox service plus the daemon protocol, wake sockets and the pods' daemon registrations |
| `internal/multica/devpods`, `cmd/sandbox-devpods` | Development only: a simulated pod side, to run the core and the task source against a real Multica without pods (`deploy/devpods/run.sh`) |
| `internal/multica/daemonwire` | The daemon protocol types we exchange with Multica, pinned to v0.11.0 |
| `internal/policy` | The policy: fair sharing, quota, placement by kind and key, holds, idle lifetime, budget deletion; knows no Kubernetes or Multica |
| `internal/sandbox` | The sandbox service: creates pods from the pod template, watches them, records what each serves on it, deletes them |
| `deploy/` | Dockerfiles, the pod image's contents (`SANDBOX.md`), Kubernetes manifests, the local kind namespace, the end-to-end scenarios (`k8s/e2e`) and the runbook |
| `docs/` | Architecture and deployment guide |
| `GLOSSARY.md`, `docs/adr/`, `.scratch/` | Terms, decision records, and specs and issues (see AGENTS.md) |

Test with `go test -race ./...`, and end to end with `deploy/k8s/e2e/scenarios.sh` against the local kind namespace. The pod image builds Multica's stock `multica` daemon from a Multica checkout next to this repository (`../multica`, at the version the Multica server runs, v0.11.0). Build the images from the repository root:

```bash
docker build -f deploy/Dockerfile.manager -t multica-sandbox-manager:dev .
docker build -f deploy/Dockerfile.pod --build-context multica=../multica -t multica-sandbox-pod:dev .
```
