---
name: verify-sandbox-manager
description: Prove a sandbox-manager change on the real thing, the manager against Multica v0.11.0 in the local kind namespace with real pods. Use before calling any change done or handing a branch over, to pick which check a change needs, or when the kind namespace, the pod image or a manager run looks wrong.
---

# Verify the sandbox manager

The surface is a service. Multica v0.11.0 (backend http://127.0.0.1:18080, web http://127.0.0.1:13001) runs in the namespace `multica-sandbox` of the kind cluster `sandbox-dev`. The manager runs on this host, creates real pods there from the fake-agent image `multica-sandbox-pod:kube-fake` (the stock daemon from the Multica fork plus `deploy/fake/fake-claude`), and serves its admin API on loopback. Users touch it through Multica: an issue assigned to an agent on one of the manager's runtimes becomes a task on a pod. Operators touch it through the admin API and the dispatch page.

Helpers are in `.claude/skills/verify-sandbox-manager/scripts/`; run them from anywhere in the repo. Below, `$V` stands for that directory.

## Which check a change needs

| Change | Check |
| --- | --- |
| Any Go change | `$V/go-checks.sh` |
| Dispatch, pods, the daemon protocol, calls to Multica, settings, `deploy/` | `$V/go-checks.sh`, then `$V/e2e.sh smoke` |
| Restart and the dispatch record, policy (budget, holds, guarantees), pod failures, owner; and before any handover | `$V/e2e.sh scenarios` (about 5 minutes) |
| The pod image's inputs (`deploy/Dockerfile.pod`, `deploy/pod`, `deploy/fake`, `deploy/home`) | `$V/pod-image.sh` first, then the check above |
| The admin API or the dispatch page | [features/admin-api-and-dispatch-page.md](features/admin-api-and-dispatch-page.md) |

The feature map, [features/README.md](features/README.md), says which scenario proves which behaviour.

## Prerequisites

- A non-root user with `go`, `docker`, `kubectl`, `jq`, `curl`, `openssl` and `git`.
- `kind` to create or delete the cluster. With the cluster already there, `$V/kind` stands in for it (`get clusters`, `get nodes`, `load docker-image` only); `e2e.sh` and `pod-image.sh` put it on `PATH` when `kind` is missing. For `up.sh` on an existing cluster: `PATH=$V:$PATH deploy/k8s/dev/up.sh`.
- The Multica fork for the pod image: a checkout or worktree of Multica on `security/v0.11.0`, by default `../multica` next to this repository (`MULTICA_SRC` names another). Only `pod-image.sh` reads it. The backend runs the stock `ghcr.io/multica-ai/multica-backend:v0.11.0` image, not the fork.
- The store tests need `SANDBOX_TEST_DATABASE_URL`, a Postgres the tests may create schemas in. `go-checks.sh` uses the kind namespace's manager database (`deploy/k8s/dev/.state/manager-db-url`) when it is unset.

## Launch

The kind namespace is long-lived; launching it is one-time.

1. `deploy/k8s/dev/up.sh`. It creates the cluster (real `kind` needed), runs Multica, the manager's database (`sandbox_manager` on `multica-pg`, node port 30432), the owner stand-in `sandbox-e2e-owner`, and writes `deploy/k8s/dev/.state/`. Rerunning it keeps the data and secrets. Ready means it printed `Namespace multica-sandbox is ready`.
2. `$V/pod-image.sh` builds the fake-agent image and loads it into the node.
3. `$V/doctor.sh` prints `doctor: ready`.

The manager is started per check. `e2e.sh` starts and stops its own (pods `:8796`, admin `127.0.0.1:8797`). By hand, `deploy/k8s/dev/manager.sh` runs one (pods `:8792`, admin `127.0.0.1:8793`; see the admin feature). `deploy/devpods/run.sh` runs one on simulated pods (admin `127.0.0.1:8791`).

## Doctor

`$V/doctor.sh` is read-only. Run it first, and again whenever anything looks off. It checks the tools, the node container, the context, Multica's three workloads and the v0.11.0 backend image, `/health`, the `.state` files, the dev-user's right to create pods, that no managed pod is left in the namespace, that the node holds the same pod image as the host and that it is newer than its inputs, the fork branch, and that the ports 8791 to 8797 are free. Every `FAIL` names its fix; exit 0 means ready.

## Drive

`$V/e2e.sh smoke` or `$V/e2e.sh scenarios`. Each run signs in a bot account of its own, creates its workspaces and agents in Multica, a database of its own, starts the manager, assigns issues to fake-claude agents through Multica's API, and waits for the task runs to complete, as a member would. Settings for another namespace are in `deploy/k8s/e2e/README.md`.

A run refuses to start while the namespace holds managed pods or something listens on its admin port: another manager may be serving them. Run one check at a time per namespace; two runs at once take over each other's pods.

Drive only the kind backend at `127.0.0.1:18080`. Any other Multica on the host (such as a Docker Compose project `multica-selfhost` and its Postgres) is not part of verification.

## Evidence

`e2e.sh` writes to `${VERIFY_EVIDENCE:-${TMPDIR:-/tmp}/verify-sandbox-manager}/<UTC time>-<check>/`:

- `run.log`: every step, each `PASS:` line with what it observed, and the teardown;
- `exit-code`, `head` (the commit and dirty files), `leftovers` (managed pods and listeners after the run; both `none` on a clean run);
- `state/`: `manager.log`, `audit.jsonl` (pod creates, deletes, leases), `pods-seen`, `pod-ids`, `multica-dump` (what Multica showed, for the privacy check), the pod template and overrides. The run's secrets and the manager binary are removed.

A proof is the `PASS:` lines of the behaviour changed plus the exit code, from a run at the commit you hand over. The scenarios observe the real side effects: task runs and comments in Multica, pods in Kubernetes, the dispatch record in Postgres across a restart. Quote the `PASS:` lines and the evidence path in your report.

## Cleanup

`e2e.sh` cleans up after itself, passed or failed: it stops its manager, deletes every managed pod and waits until they are gone, archives its agents, deletes its workspaces and database, and `leftovers` shows it. The evidence directory stays.

After a manual `manager.sh` run, stop the PID you started and delete its pods (see the admin feature); never kill by process name. `deploy/k8s/dev/down.sh` deletes the cluster and `.state`; that is not cleanup, it ends the namespace.

## Helpers

| Command | What |
| --- | --- |
| `$V/doctor.sh` | Read-only readiness check; exit 1 on any `FAIL` |
| `$V/go-checks.sh` | `gofmt`, `go vet`, `go test -race -count=1 ./...` with the store tests against Postgres; fails if they skip |
| `$V/e2e.sh smoke\|scenarios` | The end-to-end check with evidence and a leftovers check |
| `$V/pod-image.sh` | Build `multica-sandbox-pod:kube-fake` from `MULTICA_SRC` and load it into kind |
| `$V/kind` | Stand-in for the `kind` CLI on an existing cluster |
