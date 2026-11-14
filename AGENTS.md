# Sandbox Manager Instructions

This directory is our sandbox manager: the dispatcher that lends pooled pods to Multica tasks, the sandbox service that runs the pods, and the pod supervisor. It is kept apart from Multica's code. Paths below are relative to `sandbox-manager/`.

## Boundaries

- All our code, deploy files and docs live under `sandbox-manager/`. Do not add any of it under the repo's `server/`, `packages/` or `apps/`, and do not change Multica's files for the manager's sake.
- The Go module `sandbox-manager` must not require a Multica module; `module_test.go` enforces it. Daemon protocol types we exchange with Multica live in `internal/daemonwire`, pinned to the v0.6.4 wire format by its fixtures.
- `internal/sandbox` imports nothing else of this module (`internal/sandbox/imports_test.go`).

## Checks

- Go: `gofmt -l .`, `go vet ./...` and `go test -race ./...` in `sandbox-manager/`, as a non-root user.
- End to end: build the images (see `deploy/pool/compose/smoke/smoke.sh`), then run `deploy/pool/compose/smoke/scenarios.sh` and `smoke.sh` against `ghcr.io/multica-ai/multica-backend:v0.6.4`. They use their own compose projects; tear them down with `down -v`.

## Agent skills

### Issue tracker

Issues and specs live as local markdown under `.scratch/`. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five roles: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` in this directory. See `docs/agents/domain.md`.
