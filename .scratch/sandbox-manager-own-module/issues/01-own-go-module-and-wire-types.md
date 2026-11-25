# 01: Own Go module and wire types

**What to build:** All our Go code lives in the new module `sandbox-manager` under `sandbox-manager/`. The module has no requirement on `github.com/multica-ai/multica`. See the spec's target layout and Implementation Decisions.

- **Move** with `git mv`, one package at a time:
  - `server/cmd/sandbox-manager` → `sandbox-manager/cmd/sandbox-manager`
  - `server/cmd/pod-supervisor` → `sandbox-manager/cmd/pod-supervisor`
  - `server/internal/sandboxmanager` (root files) → `sandbox-manager/internal/dispatcher`, with the package renamed to `dispatcher` and `sandboxmanager_test` to `dispatcher_test`
  - `server/internal/sandboxmanager/lending` → `sandbox-manager/internal/lending`
  - `server/internal/sandboxmanager/sandbox` → `sandbox-manager/internal/sandbox`
  - `server/internal/podsupervisor` → `sandbox-manager/internal/podsupervisor`
- **`go.mod`.**
  - It has the same `go` directive as `server/go.mod`.
  - It requires only `github.com/google/uuid`, `github.com/gorilla/websocket` and `gopkg.in/yaml.v3`, at the versions `server/go.mod` has now, plus whatever `go mod tidy` adds as indirect.
  - Commit `go.sum`.
- **Wire types.** `internal/daemonwire` replaces the six `server/pkg/protocol` uses in `podapi.go`, `pool_wake.go` and `upstream_client.go`:
  - `Message`
  - `DaemonHeartbeatAckPayload`
  - `DaemonHeartbeatPendingModelList`
  - `TaskAvailablePayload`
  - `EventDaemonTaskAvailable`
  - `EventDaemonWorkspacesChanged`

  Define them from v0.6.4's `server/pkg/protocol` (`git show origin/slim/v0.6.4:server/pkg/protocol/messages.go` and `events.go`), with only the fields we use. Golden JSON fixtures in `internal/daemonwire/testdata/` come from the v0.6.4 shapes.
- **Tests that build binaries** (`podsupervisor/supervisor_test.go`, `sandbox/supervisor_test.go`) build `sandbox-manager/cmd/pod-supervisor`.
- **Boundary tests.**
  - A new test fails if `go list -m all` in the module lists any `multica-ai` module.
  - `sandbox/imports_test.go` keeps its rule: no import of `dispatcher`, `lending` or `daemonwire`, and none of `multica-ai`.
- **Leave alone:**
  - the deploy files and Dockerfiles (ticket 02), which will not build between this ticket and 02;
  - the docs and tracker (ticket 03);
  - every Multica file, `server/go.mod` included.
  - No alias, symlink or forwarding package stays at the old paths.

**Blocked by:** none

**Status:** resolved

- [x] `server/` contains no file from the four moved directories; `git log --follow` on a moved file shows its history
- [x] `sandbox-manager/go.mod` exists, and `go list -m all` there lists no `multica-ai` module; a test enforces it
- [x] No Go file in the module imports `github.com/multica-ai/...`
- [x] `daemonwire` round-trip tests pass against v0.6.4 fixtures for all six types; any v0.6.4/v0.7.0 difference in a used field is noted in Comments
- [x] The package formerly `sandboxmanager` is `dispatcher`, and exported identifiers are unchanged
- [x] The number of passing tests (`go test -race -count=1 -json`) equals the count on the old packages before the move, plus the new tests, with no test skipped
- [x] `gofmt -l`, `go vet ./...`, `go test -race ./...` pass in `sandbox-manager/`, as uid 1000
- [x] `go build ./...` and `go vet ./...` still pass in `server/`

## Comments

**Verified** (commit `e4081667e`, as uid 1000):

- **The move.** All four trees moved with `git mv`, and every file shows as a 100% rename. `server/` holds none of them. `go build ./...` and `go vet ./...` still pass in `server/`.
- **The module.** `sandbox-manager/go.mod` requires only `github.com/google/uuid` v1.6.0, `github.com/gorilla/websocket` v1.5.3 and `gopkg.in/yaml.v3` v3.0.1. `TestTheModuleRequiresNoMulticaModule` (`module_test.go`) checks `go list -m all`. No Go file imports `github.com/multica-ai/...`.
- **The import rule.** `sandbox/imports_test.go` now refuses every other `sandbox-manager/...` package as well as `multica-ai`.
- **Wire types.** `internal/daemonwire` defines the six types and constants from v0.6.4's `server/pkg/protocol`. The ack keeps only `pending_model_list`, the one field the dispatcher reads. The fields we use are identical in v0.6.4 and v0.7.0. Four round-trip tests run against fixtures in `testdata/`.
- **Tests.** Before the move, the old packages had 411 passing tests, subtests included, with none failing or skipped. After it, the module has 416: the same 411 names plus the 5 new ones. `gofmt -l` and `go vet ./...` are clean, and `go test -race ./...` passes.

**Decisions:**

- The package `sandboxmanager` is now `dispatcher`, with exported names unchanged.
- The module-root test package is named `module_test`; that rename landed with ticket 02's commit.
- gofmt sorts our `sandbox-manager/...` imports among the third-party ones, because the module path has no dot.
