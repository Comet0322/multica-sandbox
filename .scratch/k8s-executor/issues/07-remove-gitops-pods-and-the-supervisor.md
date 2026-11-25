# 07: Remove GitOps pods and the supervisor

**What to build:** What only served GitOps pods is gone, and the settings are documented.

- **Remove:**
  - `internal/podsupervisor` and `cmd/pod-supervisor`;
  - the `/pod/register` and `/pod/control` endpoints;
  - the GitOps reset and free-set code in `internal/sandbox`;
  - the GitOps pool manifests and the Compose stack;
  - the removed settings in the spec's "Settings" section, after checking the list against `cmd/sandbox-manager`.
- **Overrides:** an overrides file naming a removed setting, such as `rule`, is read with a warning.
- **Admin API:** `/v1/inventory`, `/v1/leases` and `/v1/dispatch` keep the fields that still mean something, and pods gain kind, department, key, phase and failure reason.
- **Dispatch page:** shows each pod's kind and state (warm, starting, running, held, idle, failed), and pod creation failures with their reason.
- **Docs:** the deployment guide lists every removed setting and endpoint, and describes the pool and clean runtimes for departments, including that pool pods are shared within a department and that a handoff across pods needs a push.

**Blocked by:** 03, 05, 06

**Status:** resolved

- [x] No code, manifest or doc still refers to the supervisor, resets, slots, bindings or GitOps pods as current
- [x] An old overrides file with `rule` loads with one warning
- [x] The dispatch page test still finds its 10-second `v1/dispatch` poll, and shows the new pod states
- [x] `gofmt`, `go vet` and `go test -race ./...` pass

## Comments

Branch `ticket/06-07`, commit `f69c01b`.

**Removed:** `internal/podsupervisor`, `cmd/pod-supervisor`, `internal/sandbox/{gitops,control}.go` and their tests, `/pod/register` and `/pod/control` (and `DaemonAPIConfig.PodPaths`), `Notifier.Interrupted` / `core.PodInterrupted` / `policy.PodInterrupted`, `sandbox.PodResetting` (now `PodStarting`), `AuditReset`, the affinity rules (`multica.AffinityRule`; `affinity.go` became `key.go`), `deploy/pool/k8s`, `deploy/pool/compose` (smoke and scenarios included; nothing else depended on them but `deploy/devpool/run.sh`, which now uses the kind namespace's Multica), `deploy/pool/entrypoint.sh`, `deploy/GITOPS.md`. The manager-side settings checked against `cmd/sandbox-manager`: all eleven in the spec's "Removed" list are gone from `manager.Config` (drain timeout and maximum lease time keep their built-in defaults, 10m and 2h); each one still set logs one warning at start.

**Decisions:**
- `SANDBOX_POD_TEMPLATE` is required; `manager.New` refuses a config without `Kube` unless the pod side is simulated.
- **Overrides:** `placement`, `rule` and `max_lifetime` are stripped before the strict decode; each entry naming any gives one line in `override_problems` ("…: rule no longer used (ADR 0009); ignored"), logged once like the other problems; the entry's other fields apply.
- **Admin API:** pods have `phase` (warm|starting|running|held|idle|failed, replacing `state`), `kind`, `workspace_id`, `key`, `task_id`, `hold_until`, `hold_remaining_seconds`, `idle_since`, `reason`, `created_at` (was `registered_at`); `binding` is gone. Inventory counts are per phase plus `max_pods` and `problem`; `expected_size`, `idle/leased/resetting/lost` counts and the workspaces' `placement` and `max_lifetime` are gone. `/v1/dispatch`'s `pool` has `max, live, warm, starting, running, held, idle, failed, problem`. The page shows each phase, the failure reason on a failed pod, and a banner with the creation problem; it still polls `v1/dispatch` every 10 s.
- A pod deleted on purpose leaves the inventory at once; only a lost pod stays listed as failed (with its reason) for the lost retention.
- `KubeService.Create` lends the pod that has been ready longest (was: created first).
- `policy.DefaultPodWait` stays only as the policy's fallback: the manager passes its pods' start timeout, so a task whose pod was lost before it started waits as long as a new pod may take to start.
- The pod image's default ENTRYPOINT is `tini -- multica-pod-entrypoint-kube`; the image no longer compiles this repo's Go code. Both templates still set the same command. `multica-sandbox-pool:kube-fake` was rebuilt and loaded into kind.
- devpool: a handed-back simulated pod is replaced by a new one that starts for `-start` (was `-reset`); `-capacity`, `-placement` and `-lifetime` are gone.
- Manager tests now run on the Kubernetes service against a fake clientset (`kube_harness_test.go`); GitOps-only tests (resets, supervisor events, registration, slots) were deleted or rewritten to deletion, idle, failure-reason and creation-failure cases.

**Verified in kind:** a manager from `deploy/k8s/dev/manager.sh` (`pool,clean`, 3 pods, 1 warm) ran a fake-agent task (DEV-8) to completion; `/v1/dispatch` showed the pod `held` with its key and countdown and two `warm` pods; `/dispatch` answered 200. Manager stopped and pods deleted afterwards.

**Open:**
- No manager Deployment manifest yet (`deploy/pool/k8s/manager.yaml` was GitOps-specific and went with it).
- Docs were rewritten by a sub-agent (`deploy/POOL.md` from scratch); worth a read by a human, in particular `deploy/devpool/run.sh`, which passes `bash -n` but was not run.
