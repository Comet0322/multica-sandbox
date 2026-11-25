# 04: End-to-end verification and docs

**What to build:** Show that pods and Multica see no difference, and bring the operator docs in line with the new module layout.

- **End to end.** Run `deploy/sandbox/pool/compose/smoke/scenarios.sh` against `ghcr.io/multica-ai/multica-backend:v0.6.4` without editing the scenarios. The backend's request log must show no 4xx or 5xx, as in the previous spec's ticket 13.
- **Docs.**
  - `deploy/sandbox/POOL.md` and `SANDBOX.md` describe the manager as dispatcher plus sandbox service. They say which settings belong to which, and how reset, loss and eviction flow now. Remove anything that describes the old in-dispatcher pod handling.
  - Package comments of `sandbox`, `lending` and the manager's `Pool` match the code.
- **Environment.** Use only the scenario driver's own compose projects, and tear them down with `down -v`. Never touch `multica-postgres-1` or `multica-selfhost`.

**Blocked by:** 03

**Status:** resolved

- [x] All eight scenarios pass against v0.6.4 with no backend 4xx/5xx
- [x] The compose smoke (`smoke.sh`) passes
- [x] The k8s manifests and compose files are unchanged, or any change is justified in the comments
- [x] POOL.md, SANDBOX.md and the package comments describe the sandbox service and no longer describe pod handling inside the dispatcher
- [x] `git diff --check` is clean

## Comments

**Verified** (implementer run on `6c66feb42` with this ticket's docs commit `513686f9d`):

- **Scenarios.** The manager image `multica-sandbox-manager:smoke` and the pod image `multica-sandbox-pool:smoke-fake` were built from this tree. `deploy/sandbox/pool/compose/smoke/scenarios.sh` was run unchanged against `ghcr.io/multica-ai/multica-backend:v0.6.4`. It exited 0, and all eight scenarios passed: registration, identity, reset, queue, quota, hold, eviction and lifetime. The driver's upstream check found no backend 4xx or 5xx.
- **Smoke.** `smoke.sh` exited 0: a pod registered, ran a task, was reset and was lent again.
- **Cleanup.** The compose projects `pool-scenarios` and `pool-smoke` were torn down with `down -v`. `multica-postgres-1` and `multica-selfhost` were not touched.
- **Manifests.** The k8s manifests and compose files are unchanged. No code or scenario changed in this ticket.
- **Docs.**
  - `POOL.md` has a new section, "Two modules, one process", which lists the settings that belong to the sandbox service.
  - Its reset, loss, registration, audit and dispatch text now follows the split.
  - It documents ticket 03's behaviour changes.
  - `SANDBOX.md` changed by one line.
  - The package comments already matched the code.
- **No bugs found.**

**Left for code review:** the POOL.md line under "Claim load and dispatch rate" still says each tick hands out at most one task. That is still true: `claimRound` returns after the first claimed task, and only pod acquisition can take several pods in one tick. It should be confirmed against the code.
