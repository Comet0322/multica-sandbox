# Sandbox Manager Instructions

This repository is our sandbox manager: the dispatcher that lends pooled pods to Multica tasks, the sandbox service that runs the pods, and the pod supervisor. Multica itself is a separate checkout next to it (`../multica`), used only to build the stock daemon into the pod image; do not change it for the manager's sake.

## Boundaries

- The Go module `sandbox-manager` must not require a Multica module; `module_test.go` enforces it. Daemon protocol types we exchange with Multica live in `internal/daemonwire`, pinned to the v0.6.4 wire format by its fixtures.
- `internal/sandbox` imports nothing else of this module (`internal/sandbox/imports_test.go`).

## Checks

- Go: `gofmt -l .`, `go vet ./...` and `go test -race ./...`, as a non-root user.
- End to end: build the images (see `deploy/pool/compose/smoke/smoke.sh`; the pod image needs `--build-context multica=../multica`), then run `deploy/pool/compose/smoke/scenarios.sh` and `smoke.sh` against `ghcr.io/multica-ai/multica-backend:v0.6.4`. They use their own compose projects; tear them down with `down -v`.

## Agent skills

### Issue tracker

Issues and specs live as local markdown under `.scratch/`. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five roles: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
