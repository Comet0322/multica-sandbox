Status: resolved

# Sandbox manager in its own top-level module

## Problem Statement

Our sandbox manager is spread through Multica's own tree:

- **Go code** sits in Multica's module, under Multica's directories:
  - `server/cmd/sandbox-manager`, `server/cmd/pod-supervisor`
  - `server/internal/sandboxmanager/**`, `server/internal/podsupervisor`
- **Deployment files** are in `deploy/sandbox/`.
- **Domain docs and tracker files** are at the repo root, where Multica's would be:
  - `GLOSSARY.md`, `docs/adr/`, `docs/agents/`
  - `.scratch/`, `AGENTS.md`, `.gitignore`

A reader cannot tell which files are Multica and which are ours. Importing a new Multica release means merging around our files in Multica's directories. Nothing stops our code from importing Multica packages, and the dispatcher already does: it uses `server/pkg/protocol`.

Two facts make a clean cut possible:

- **Our code needs very little from Multica.** Its only Multica import is six types from `server/pkg/protocol`, in `podapi.go`, `pool_wake.go` and `upstream_client.go`:
  - `Message`
  - `DaemonHeartbeatAckPayload`
  - `DaemonHeartbeatPendingModelList`
  - `TaskAvailablePayload`
  - the event names `EventDaemonTaskAvailable` and `EventDaemonWorkspacesChanged`
- **Multica needs nothing from us.** No Multica code imports our packages.

## Solution

Everything that is ours moves under one top-level directory, `sandbox-manager/`, with its own Go module `sandbox-manager`. The module does not require `github.com/multica-ai/multica`. The six protocol types become our own wire types, pinned by JSON contract tests against the v0.6.4 wire format.

Nothing changes at runtime:

- pod behaviour;
- image contents;
- every HTTP endpoint;
- environment variable names and defaults;
- the k8s manifests' resources and image names.

Only the source locations, the Go import paths and the image build steps change.

Target layout:

```
sandbox-manager/
  go.mod                       module sandbox-manager, no multica-ai requirement
  AGENTS.md  CLAUDE.md         agent instructions for this tree
  README.md                    what this is, how to test and build
  GLOSSARY.md  docs/adr/  docs/agents/
  .scratch/                    specs and tickets
  cmd/sandbox-manager/         was server/cmd/sandbox-manager
  cmd/pod-supervisor/          was server/cmd/pod-supervisor
  internal/dispatcher/         was server/internal/sandboxmanager (package sandboxmanager)
  internal/lending/            was server/internal/sandboxmanager/lending
  internal/sandbox/            was server/internal/sandboxmanager/sandbox
  internal/podsupervisor/      was server/internal/podsupervisor
  internal/daemonwire/         our copy of the daemon wire types
  deploy/                      was deploy/sandbox (Dockerfiles, compose, k8s, smoke, docs)
```

The repo root keeps only short pointers:

- **`AGENTS.md`** says that `sandbox-manager/` is ours and that its instructions are in `sandbox-manager/AGENTS.md`.
- **`.gitignore`** keeps the tracker and agent docs tracked at their new paths.

## User Stories

1. As a maintainer, I want every file we wrote under one directory, so that I can tell ours from Multica's at a glance.
2. As a maintainer, I want `server/` to hold no code of ours, so that importing a Multica release does not touch our files.
3. As a developer, I want our Go module to have no requirement on Multica's module, so that the compiler forbids a Multica import.
4. As a developer, I want a test that fails if `go list -m all` in our module lists any `multica-ai` module, so that nobody adds the requirement back by accident.
5. As a developer, I want the daemon wire types we exchange with Multica defined in our module, so that the dispatcher compiles without Multica.
6. As a developer, I want each of those wire types checked against a golden JSON fixture of the v0.6.4 format, so that a field rename breaks a test and not a pod.
7. As a developer, I want the dispatcher package named `dispatcher`, so that the code uses the name the glossary and ADR 0004 use.
8. As a developer, I want the sandbox service's import rule to stay enforced, so that `sandbox` still imports nothing from `dispatcher`, `lending` or `daemonwire`.
9. As a developer, I want every existing test to move with its code and pass in the new module, with the same count, so that the move is proven to be behaviour-neutral.
10. As an operator, I want the pod image to contain the same binaries, built from the same sources, so that pods need no redeploy for behaviour reasons:
    - the stock `multica` daemon, from `server/`;
    - `pod-supervisor`, from our module.
11. As an operator, I want the manager image built from our module alone, so that its build does not depend on Multica's sources.
12. As an operator, I want the k8s manifests and compose files to keep their resources, image names and settings, so that GitOps overlays keep working. Only paths inside our tree may change.
13. As an operator, I want `scenarios.sh` and `smoke.sh` to pass against `ghcr.io/multica-ai/multica-backend:v0.6.4` from their new location, so that the move is proven end to end.
14. As a maintainer, I want the glossary, ADRs, agent docs and the tracker inside `sandbox-manager/`, so that the repo root carries no domain docs of ours.
15. As a coding agent, I want the root `AGENTS.md` to point me at `sandbox-manager/AGENTS.md`, so that I still find the tracker and domain docs.
16. As a maintainer, I want an ADR recording why the manager lives in its own module, so that nobody moves it back into `server/`.
17. As a maintainer, I want the old paths removed with no aliases, symlinks or forwarding packages, so that there is one place for each file.

## Implementation Decisions

- **Directory and module name.** Both are `sandbox-manager`. The module path has no domain, because the module is not fetched by others. If it moves to its own repository later, the path changes then.
- **Moves use `git mv`,** so that history follows each file. Each package moves whole; do not combine moving and editing in a way that loses rename detection.
- **Package names.**
  - `sandboxmanager` becomes `dispatcher`, and its external test package becomes `dispatcher_test`.
  - `lending`, `sandbox` and `podsupervisor` keep their names.
  - Exported identifiers keep their names.
- **Wire types (`internal/daemonwire`).**
  - Define only the types and fields the dispatcher reads or writes, with the JSON tags of the v0.6.4 wire format.
  - Golden fixtures under `testdata/` are written from v0.6.4's `server/pkg/protocol`, the version the company server runs. This tree's `server/` is v0.7.0. Where v0.6.4 and v0.7.0 differ in a field we use, follow v0.6.4 and note it in the ticket comments.
  - Tests decode each fixture into our type and encode it back. The fields we use must survive the round trip.
- **Self-built tests.** The tests that build `pod-supervisor` with `go build` use our module's import path.
- **The module boundary test.**
  - A test in the module runs `go list -m all` and fails on any `multica-ai` module.
  - The sandbox package's `imports_test.go` keeps its rule, rewritten for the new import paths.
- **Go version and dependencies.**
  - The `go` directive is the same as `server/go.mod`.
  - The requirements are only what our code needs: `github.com/google/uuid`, `github.com/gorilla/websocket` and `gopkg.in/yaml.v3`, with their current versions from `server/go.mod`.
  - `server/go.mod` is left as it is.
- **Images.**
  - Both Dockerfiles build from the repo root as now.
  - `Dockerfile.pool` builds `multica` from `server/` and `pod-supervisor` from `sandbox-manager/`.
  - `Dockerfile.manager` copies only `sandbox-manager/`.
  - The `.dockerignore` files allow exactly the paths each build copies.
  - The resulting image layout, users, entrypoint and environment are unchanged.
- **Deployment files.**
  - `deploy/sandbox/**` moves to `sandbox-manager/deploy/**` with its internal layout kept, so relative paths inside it keep working.
  - Comments and scripts that name the repo-relative Dockerfile path are updated.
  - Kustomize overlays elsewhere reference `base/pool/k8s` in their own repository, so they are unaffected (see GITOPS.md).
- **Docs and tracker.**
  - These move into `sandbox-manager/`: `GLOSSARY.md`, `docs/adr/0001`–`0004`, `docs/agents/*`, `.scratch/README.md` and every `.scratch/<feature>/` directory, this spec included.
  - The current root `AGENTS.md` content moves to `sandbox-manager/AGENTS.md`, with its paths rewritten. A `sandbox-manager/CLAUDE.md` imports it.
  - The root `AGENTS.md` becomes a pointer of a few lines.
  - The root `.gitignore` un-ignores `/sandbox-manager/.scratch/` and `/sandbox-manager/docs/agents/` in place of the old paths.
  - Path references inside moved docs (POOL.md, SANDBOX.md, READMEs, ADRs, the glossary) are rewritten to the new paths. Earlier specs' and tickets' Comments are history and keep their old paths.
- **New ADR 0005:** "The sandbox manager lives in its own module". It covers:
  - why the manager is split from Multica's tree and module;
  - the option of a separate repository, and why it was deferred;
  - the wire-type copy and its contract tests as the price.
- **Out of the move.** These are not ours to move, or are Multica changes: the untracked `.codegraph/`, `deploy/k8s-poc/` and `multica-best-practices/`, and the branch's changes to Multica files (`server/cmd/multica`, `packages/views`, `server/go.mod`).

## Testing Decisions

- A good test here checks behaviour at a module's seam, not file locations. The existing suites already do that; they move unchanged except for import paths and package names.
- **The test count must match before and after.** Run `go test -race -count=1 -json` over the old packages before the move and the new module after it, and record both counts.
- **New tests:**
  - `daemonwire` round-trip tests against the v0.6.4 golden fixtures.
  - The module boundary test (`go list -m all`).
- **End to end:**
  - `deploy/pool/compose/smoke/scenarios.sh` (8 scenarios) against `ghcr.io/multica-ai/multica-backend:v0.6.4`, with the images built from the new Dockerfiles, and no backend 4xx or 5xx.
  - `smoke.sh` as well.
  - Use only the drivers' own compose projects and tear them down with `down -v`. Never touch `multica-postgres-1` or `multica-selfhost`.
- **Image equivalence:** the pod image built before and after has the same `/usr/local/bin` entries, user, entrypoint and environment (`docker image inspect` and a file listing).

## Out of Scope

- A separate repository or a separate process.
- Any behaviour, configuration, protocol or pod change.
- Changes to Multica's own files: `server/`, `packages/`, `apps/`, `server/go.mod`.
- A Makefile or CI pipeline for the new module.
- Rewriting history so that our commits are split from Multica's.

## Further Notes

- After this, a separate repository is a `git subtree split --prefix=sandbox-manager` away. The pod image build still needs Multica's `server/` for the stock daemon, so that repo would fetch it by tag.
- The ADRs keep their numbers; 0005 is the next.

## Comments

All three tickets are resolved on `feat/sandbox-manager-own-module`. The `CLAUDE.md`, `docs/agents/` and `.scratch/` excludes were dropped from `.git/info/exclude`; both `CLAUDE.md` files are committed and the root `.gitignore` is gone (ticket 03).
