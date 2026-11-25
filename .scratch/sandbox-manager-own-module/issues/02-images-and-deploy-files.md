# 02: Images and deploy files

**What to build:** The deploy tree lives in `sandbox-manager/deploy/`, and both images build from the new module. Runtime is unchanged. See the spec's Images and Deployment files decisions.

- **Move** `deploy/sandbox/**` → `sandbox-manager/deploy/**` with `git mv`, keeping its internal layout.
- **`Dockerfile.manager`** builds `./cmd/sandbox-manager` from `sandbox-manager/` only.
- **`Dockerfile.pool`** builds `multica` from `server/` as now, and `pod-supervisor` from `sandbox-manager/`.
  - Its `COPY` paths for `pool/pip.conf`, `pool/entrypoint.sh`, `home/` and `fake/` point at `sandbox-manager/deploy/`.
  - The image layout, users, entrypoint, environment and targets (`fake`, `agents`) are unchanged.
- **`.dockerignore` files** move with their Dockerfiles. The manager's allows only `sandbox-manager/`; the pool's allows `server/` and `sandbox-manager/`. Build output and test data stay excluded, as now.
- **Repo-relative paths** in comments and scripts are updated: the build commands in `smoke.sh`, `lib.sh`, `scenarios.sh`, the Dockerfile headers and the READMEs. Paths relative inside the deploy tree stay.
- **The k8s manifests and compose files** keep their resources, image names, settings and env names.

**Blocked by:** 01

**Status:** resolved

- [x] `deploy/sandbox/` no longer exists; `sandbox-manager/deploy/` holds the same files
- [x] Both images build from the repo root with the documented commands
- [x] Before and after the move, the pod image (`--target fake` and the default target) has the same `/usr/local/bin` entries, `User`, `Entrypoint`, `Env` and `Healthcheck` (`docker image inspect`), and the manager image the same `Entrypoint`, `User` and `Healthcheck`
- [x] `scenarios.sh` passes all eight scenarios against `ghcr.io/multica-ai/multica-backend:v0.6.4`, with no backend 4xx or 5xx, from its new location and unedited apart from path comments
- [x] `smoke.sh` passes
- [x] `git diff -M` shows the k8s and compose YAML as pure renames, or each change is explained in Comments
- [x] The driver compose projects are torn down with `down -v`; `multica-postgres-1` and `multica-selfhost` are untouched

## Comments

**Verified:**

- **The move.** `deploy/sandbox/**` moved with `git mv` to `sandbox-manager/deploy/**`, with its layout kept.
- **`Dockerfile.manager`** builds from `sandbox-manager/` only, and its `.dockerignore` lets in just `sandbox-manager/`, without `.scratch`, `docs` or `deploy`.
- **`Dockerfile.pool`** builds `multica` from `server/` and `pod-supervisor` from `sandbox-manager/`, and its `.dockerignore` lets in `server/` and `sandbox-manager/`.
- **Image equivalence.** Images were built from the commit before the move (`mv-before*`) and after it (`smoke`, `smoke-fake`, `mv-after`), for the manager, the pool's `fake` target and the default target; the default target used `AGENT_NPM_REGISTRY=https://registry.npmjs.org`.
  - `User`, `Entrypoint`, `Env`, `Healthcheck`, `Volumes` and `WorkingDir` are identical for each pair.
  - The `ls -l` of `/usr/local/bin` and `/etc/multica-sandbox` shows the same files and modes.
  - Only the total size differs, by 4 KiB, because the module path changes the pod-supervisor binary's strings.
- **Scenarios.** `scenarios.sh`, run from its new path against `ghcr.io/multica-ai/multica-backend:v0.6.4`, exited 0. All eight scenarios passed, and the driver reported that every upstream call succeeded, so there was no 4xx or 5xx.
- **Smoke.** `smoke.sh` exited 0.
- **Cleanup.** The driver projects tore themselves down with `down -v`. `multica-postgres-1` and `multica-selfhost` were not touched.
- **YAML.** The only changes in the k8s and compose YAML are comment paths, `deploy/sandbox/POOL.md` and `SANDBOX.md` becoming `sandbox-manager/deploy/...`, in `manager.yaml`, `sandbox/pool.yaml` and `overrides.yaml.example`. Resources, images and settings are unchanged.

**Also in this commit:**

- Three Go comments now point at `deploy/POOL.md`.
- The module-root test package is renamed from `sandboxmanager_test` to `module_test`.
