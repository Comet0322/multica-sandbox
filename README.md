# Sandbox manager

Lends a pool of long-lived sandbox pods to Multica tasks. The pods run Multica's stock daemon under our pod supervisor; the manager plays the Multica server toward them and claims tasks upstream with a bot token.

| Path | What |
| --- | --- |
| `cmd/sandbox-manager`, `cmd/pod-supervisor` | The two binaries |
| `internal/dispatcher` | Talks to Multica, plays its server toward the pods, lends pods |
| `internal/lending` | The lending policy: queue, quota, bindings, holds, lifetime |
| `internal/sandbox` | The sandbox service: registers, resets and loses pods |
| `internal/podsupervisor` | PID 1 in a pod: runs the daemon, resets the pod's home |
| `internal/daemonwire` | The daemon protocol types we exchange with Multica |
| `deploy/` | Dockerfiles, compose and k8s manifests, operator docs (`POOL.md`, `SANDBOX.md`, `GITOPS.md`) |
| `GLOSSARY.md`, `docs/adr/` | Terms and decisions |
| `.scratch/` | Specs and tickets |

Test with `go test -race ./...` here. Build the images from the repository root:

```bash
docker build -f sandbox-manager/deploy/Dockerfile.manager -t multica-sandbox-manager:dev .
docker build -f sandbox-manager/deploy/Dockerfile.pool -t multica-sandbox-pool:dev .
```

The pod image also builds Multica's `multica` daemon from `server/`.
