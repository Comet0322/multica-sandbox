# Sandbox Manager Instructions

This repository is our sandbox manager: the program that runs Multica tasks one at a time on pods it creates through the Kubernetes API, and the sandbox service that creates, watches and deletes those pods. Multica itself is a separate checkout next to it (`../multica`, on the company fork's branch `security/v0.11.0`), used only to build the stock daemon into the pod image; do not change it for the manager's sake.

## Where the work stands

The work is a linear stack of branches on `main`, merged bottom first. New work branches from the top.

| Branch | Holds |
| --- | --- |
| `pod-failure-handling` | Pod failures classified, the broken state, the pods' owner Deployment; ADRs and `.scratch/` in git |
| `multica-v0.11-compat` | Multica v0.11.0 only: the manager answers the pods' environment reports, refuses a wake socket with an Origin; kind and e2e on v0.11.0 |
| `models-from-env` | The model picker answered from `SANDBOX_MODELS` |
| `rename-pod-image` | `deploy/Dockerfile.pod` and the image `multica-sandbox-pod`; simulated pods named devpods |
| `two-services-ha-step1` (top) | The dispatch record in Postgres (`internal/store`), one daemon task slot per pod, the rollout evaluation plan |

Other local branches are already merged into `main`.

- Active effort: `.scratch/two-services-with-ha/`, the manager and the sandbox service as two services, each highly available. Read `map.md` first. The Multica v0.11 compatibility and step 1 are done; next is step 2, leader election with two manager replicas (issue 01 holds the proposal, no brief yet).
- Once deployed in the company: `.scratch/rollout-evaluation/plan.md`, how to load-test, tune and judge the manager against one fixed runtime per department, baseline first.

## Boundaries

- The Go module `sandbox-manager` must not require a Multica module; `module_test.go` enforces it. Daemon protocol types we exchange with Multica live in `internal/multica/daemonwire`, pinned to the v0.11.0 wire format by its fixtures.
- `internal/core` decides which pod runs which task and keeps the record of every task dispatched. It knows tenants, opaque tasks and pod IDs, and imports only the policy, `internal/policy` (`internal/core/imports_test.go`). It reaches the rest through its `TaskSource`, `Pool` and `Store` interfaces (`internal/core/ports.go`); test it with fakes of them.
- Everything Multica-specific lives under `internal/multica`. The package itself imports only the policy (`internal/multica/imports_test.go`). `internal/multica/tasksource` (the daemon Multica sees) and `internal/multica/pool` (the sandbox service plus the daemon protocol served to the pods) are the core's two sides; neither imports the other, and what passes between them directly (registrations, model list requests, workspace hints) never touches the policy. `internal/manager` wires them together.
- `internal/policy` imports nothing else of this module and knows no Kubernetes or Multica: a workspace is a tenant, and a task's key (its issue, else its chat session) comes from the caller (`internal/policy/imports_test.go`).
- `internal/sandbox` imports nothing else of this module (`internal/sandbox/imports_test.go`).
- `internal/store` keeps the core's dispatch record in Postgres (`core.Store`) and imports only the core and the policy (`internal/store/imports_test.go`).

## Checks

- Prove every change with the `verify-sandbox-manager` skill (`.claude/skills/verify-sandbox-manager/`) before calling it done. Its table names the check each change needs, from the Go checks to the end-to-end scenarios on real pods in the local kind namespace, and its prerequisites: the kind namespace, the pod image built from `security/v0.11.0`, and `SANDBOX_TEST_DATABASE_URL` for the store tests.
- Before handing a branch over: the skill's `e2e.sh scenarios` green at its head, the `security-scan` skill (user-level, not in this repo) with its findings triaged, and `/code-review` of the branch against its base.
- Leave no pod, manager or watcher running after a manual run.

## Agent skills

### Issue tracker

Issues and specs live as markdown under `.scratch/`, committed with the code so the next agent finds them (see `docs/agents/issue-tracker.md`).

### Triage labels

Default five roles: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`. The glossary, the ADRs and `.scratch/` are all committed.
