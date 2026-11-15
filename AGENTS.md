# Sandbox Manager Instructions

This repository is our sandbox manager: the program that runs Multica tasks one at a time on pods it creates through the Kubernetes API, and the sandbox service that creates, watches and deletes those pods. Multica itself is a separate checkout next to it (`../multica`), used only to build the stock daemon into the pod image; do not change it for the manager's sake.

## Boundaries

- The Go module `sandbox-manager` must not require a Multica module; `module_test.go` enforces it. Daemon protocol types we exchange with Multica live in `internal/multica/daemonwire`, pinned to the v0.6.4 wire format by its fixtures.
- `internal/core` decides which pod runs which task and keeps the record of every task dispatched. It knows tenants, opaque tasks and pod IDs, and imports only the policy, `internal/policy` (`internal/core/imports_test.go`). It reaches the rest through its `TaskSource` and `Pool` interfaces (`internal/core/ports.go`); test it with fakes of them.
- Everything Multica-specific lives under `internal/multica`. The package itself imports only the policy (`internal/multica/imports_test.go`). `internal/multica/tasksource` (the daemon Multica sees) and `internal/multica/pool` (the sandbox service plus the daemon protocol served to the pods) are the core's two sides; neither imports the other, and what passes between them directly (registrations, model list requests, workspace hints) never touches the policy. `internal/manager` wires them together.
- `internal/policy` imports nothing else of this module and knows no Kubernetes or Multica: a workspace is a tenant, and a task's key (its issue, else its chat session) comes from the caller (`internal/policy/imports_test.go`).
- `internal/sandbox` imports nothing else of this module (`internal/sandbox/imports_test.go`).

## Checks

- Go: `gofmt -l .`, `go vet ./...` and `go test -race ./...`, as a non-root user.
- End to end: the local kind namespace in `deploy/k8s/dev` (`up.sh`; see `docs/DEPLOYMENT.md`) runs Multica v0.6.4 and real pods. `deploy/k8s/e2e/scenarios.sh` (or the quicker `smoke.sh`) runs the manager against it and cleans up after itself; `deploy/k8s/dev/manager.sh` runs it by hand. Build the fake pod image with `--build-context multica=../multica --target fake` and load it into kind after any change to `deploy/`. Leave no pod or manager running after a manual run.

## Agent skills

### Issue tracker

Issues and specs live as local markdown under `.scratch/`, which is never committed (see `docs/agents/issue-tracker.md`).

### Triage labels

Default five roles: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`. The ADRs, like `.scratch/`, stay local and are never committed; the glossary is committed.
