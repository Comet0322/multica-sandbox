# Deploy

What the manager and its pods are built and deployed from. How it fits
together is in [ARCHITECTURE.md](../docs/ARCHITECTURE.md); how to run it is in
the [deployment guide](../docs/DEPLOYMENT.md).

| Path | What |
|---|---|
| `Dockerfile.manager` | The manager's image |
| `Dockerfile.pool` | The pods' image: `tini`, the stock daemon and the agent CLIs (see [SANDBOX.md](SANDBOX.md)) |
| `pool/`, `home/` | The pod image's entrypoint, package mirror settings and read-only agent configuration |
| `fake/` | A stand-in agent CLI for tests without a model (the image's `fake` target) |
| `k8s/` | Multica's manifests, the example pod template, the manager's RBAC and Deployment, the local kind namespace and the end-to-end scenarios (see [k8s/README.md](k8s/README.md)) |
| `devpool/` | Development only: the manager on simulated pods against a real Multica, with load, to watch the dispatch page |
| [RUNBOOK.md](RUNBOOK.md) | Moving a department onto the manager |
