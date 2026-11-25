# 11: Per-workspace overrides

**What to build:** Workspaces can differ where they need to. The pool has defaults for the affinity rule, hold timeout, maximum lifetime and quota. The manager's configuration can override any of them per workspace, keyed by workspace slug. Pod capacity stays pool-wide. A reload applies changed overrides without a restart.

**Blocked by:** 08, 10

**Status:** resolved

- [x] A workspace without an override uses the pool defaults
- [x] Overriding the affinity rule, hold timeout, maximum lifetime or quota changes the behaviour for that workspace only
- [x] An override for an unknown slug or a bad value is reported clearly and does not stop the other workspaces
- [x] A reload applies changed overrides to new placements without disturbing running tasks

## Comments

**Verified** (from `server/`, as uid 1000, on integration tip `188ca1908`; merging it again brought nothing):

- `gofmt -l` (clean) and `go vet` on `./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/` pass.
- `go test -count=1 ./internal/sandboxmanager/... ./cmd/sandbox-manager/`: 349 passed. `go test -race -count=1 ./internal/sandboxmanager/...`: 323 passed.
- `./internal/podsupervisor/`: `TestSingleTaskLocationsAreUnderTheDaemonHome` is flaky. It reads an empty daemon env file and fails in most runs (4 of 5 with `-count=5`), though it sometimes passes alone. This ticket does not touch `internal/podsupervisor` or `cmd/pod-supervisor`, so the flake was already there. The other 25 supervisor tests pass.
- Module seam: `lending/overrides_test.go` (`TestPerWorkspaceOverrides`, fake clock). It covers:
  - a workspace's own maximum lifetime applying to its bindings only;
  - a workspace lifetime of 0 meaning none;
  - a changed lifetime applying to bindings opened afterwards and not failing running tasks;
  - a changed quota applying to the next claim;
  - a changed rule keying the next task by the new rule.
- HTTP seam: `overrides_test.go`, with a real Sync and a fake server that has slugs on `/api/workspaces` only.
  - `TestWorkspaceWithoutAnOverrideUsesThePoolDefaults`: the inventory shows the overrides for one workspace and the pool defaults for the other.
  - `TestQuotaOverrideCapsOnlyItsWorkspace` and `TestHoldOverrideAppliesToItsWorkspaceOnly`: the behaviour changes for that workspace only.
  - `TestBadOrUnknownOverridesAreReportedAndDoNotStopOthers`: a bad duration, an unknown slug, an unknown field and a bad rule are each logged and listed, and the valid entry applies.
  - `TestAnUnreadableOverridesFileKeepsTheCurrentOverrides`: invalid YAML and a read error keep the overrides in effect, and an emptied file drops them.
  - `TestReloadAppliesChangedOverridesToNewPlacementsOnly`: the changed workspace is in `Updated`, `Unapplied` is empty, and the next claim obeys the new quota. The running lease and its report are untouched.
  - `TestOverridesFileFormat`: bare `0` durations, JSON, empty entries, and a one-line "missing unit" problem.
- Command: `cmd/sandbox-manager/overrides_env_test.go`. The file is read again on every call, no setting means no reader, and a relative path is refused at start.

**Not verified:** neither the compose smoke nor the k8s manifests were run. ConfigMap propagation into a running pod was not observed.

**Config format.**

- The environment stays the only config surface. `SANDBOX_WORKSPACE_OVERRIDES_FILE` names an absolute path, read with `os.Root` like the `_FILE` secrets.
- The file is YAML (JSON works too): a mapping from workspace slug to `placement`, `hold_timeout`, `max_lifetime` and `max_leases`. The keys mirror the environment variables they override, and a field left out keeps the pool default.
- `PoolConfig.ReadOverrides func() ([]byte, error)` is the reader, and `sandboxmanager.ParseWorkspaceOverrides` is the parser.

**What changed.**

- **Hot reload.** `Sync` runs every `SANDBOX_POOL_SYNC_INTERVAL` and on SIGHUP. Before `reload`, it calls `resolveWorkspaceSettings`, which:
  1. reads the file;
  2. fills each listed workspace's settings with the pool defaults, then its slug's override;
  3. feeds them through `configureWorkspaceLocked` into `lending.SetWorkspace` (`Quota`, `Rule`, `HoldTimeout`, `MaxLifetime`).
- **Applying changes.** `workspaceSet.apply` updates rule, quota, hold and lifetime in place and reports them in `ReloadResult.Updated`. Only a runtime-name change stays `Unapplied`, and Sync never produces one. The quota default moved out of `normalizeWorkspaces` into the resolution step, so an override of `max_leases: 0` means "no cap".
- **Running tasks are not disturbed.**
  - A binding records its maximum lifetime when it opens (`binding.lifetime`), so a shortened lifetime never drains or fails a running binding.
  - The hold countdown is fixed when the hold starts.
  - Quota and rule apply only to later claims.
- **Problems are reported, never fatal.**
  - A bad entry (unknown field, bad value) keeps that slug's previous override, or the pool defaults when it had none.
  - An unknown slug is ignored.
  - An unreadable or unparsable file keeps every override in effect.
  - Each problem is logged as a WARN `workspace override not applied` line when the set of problems changes, and stays in the inventory's `override_problems` until fixed.
- **Slugs.** The v0.6.4 daemon listing has no slug (`DaemonWorkspaceResponse`), so the manager reads `GET /api/workspaces` for slugs, only while the file has entries. If that read fails, a served workspace keeps the slug it had. If a future daemon listing carries the slug, it is used directly.
- **Inventory.** Each workspace shows `slug` and the settings in effect: `placement`, `hold_timeout` and `max_lifetime` (strings) and `max_leases`.
- **Deploy.**
  - `POOL.md` has a "Per-workspace overrides" section.
  - k8s: a `sandbox-manager-overrides` ConfigMap, mounted as a directory rather than with subPath so that edits propagate.
  - compose: a directory bind mount at `${OVERRIDES_DIR:-./overrides}`, plus `overrides.yaml.example` and README steps.

**Left as is:** `Reload` in `export_test.go` stays, and its comment now says why. Tests use it for:

- membership changes without the listing debounce;
- invalid lists that Sync never produces;
- staging settings in the older harness tests.

New tests set settings through the overrides file and Sync (`newOverrideHarness`).
