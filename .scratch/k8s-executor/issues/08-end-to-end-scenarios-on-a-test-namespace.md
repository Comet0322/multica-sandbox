# 08: End-to-end scenarios on a test namespace

**What to build:** `scenarios.sh` and `smoke.sh` run against a dedicated test namespace with real pods, the stock v0.6.4 daemon and a v0.6.4 backend, replacing the Docker Compose stack. devpool follows the new pod states.

- **Scenarios:**
  - **Handoff on one pod:** agent A hands an unpushed branch to agent B on one issue, and B finds it on the held pod.
  - **Handoff across pods:** after the hold has gone, B lands on another pod, and only what A pushed is there.
  - **Clean:**
    - a clean task gets a new pod;
    - the clean pod is reused for its own issue only;
    - its resume on a new pod falls back to a fresh environment without an error.
  - **Out of memory:** a task over its limit fails with an out-of-memory reason, and the next task runs.
  - **Budget:** a full budget deletes an idle pool pod before a held one, and never takes a pod within another department's guarantee.
  - **Pod that cannot start:** a pod that cannot start leaves its task queued in Multica.
  - **Restart:** restarting the manager mid-task keeps the task.
  - **Privacy:** no pod name, hostname or IP appears in Multica.
- **devpool** simulates the new pod states.

**Blocked by:** 07

**Test namespace:** `multica-sandbox` in the local kind cluster `sandbox-dev`. `deploy/k8s/dev/up.sh` creates it, with the v0.6.4 backend, Postgres and web app inside it, and writes a kubeconfig that may only create, delete, get, list and watch pods to `deploy/k8s/dev/.state/dev-user.kubeconfig`. The scenarios take the namespace and kubeconfig as settings, so they also run against the company's namespace.

**Status:** resolved

- [x] Every scenario above passes twice in a row against the test namespace
- [x] Teardown removes every pod the run created
- [x] The README of the scenarios says how to point them at a namespace and kubeconfig

## Comments

Branch `ticket/08-09-docs`, commit `263e081`.

**What:** `deploy/k8s/e2e/scenarios.sh` and `smoke.sh` (with `lib.sh` and `README.md`). The manager runs on the host and creates real pods in the namespace with the given kubeconfig; each run signs in its own bot (`e2e-<run>@dev.test`) with two workspaces (departments a and b), and on exit stops the manager, deletes every pod labelled `app.kubernetes.io/managed-by=sandbox-manager`, archives its agents and deletes its workspaces (`KEEP=1` keeps them). Settings: `E2E_KUBECONFIG`, `E2E_NAMESPACE`, `E2E_API`, `E2E_CODE`, `E2E_MANAGER_URL`, `E2E_POD_IMAGE`, `E2E_PULL_POLICY`, ports. It refuses to start when the namespace already has managed pods.

**How the scenarios are staged:**
- Handoff: the fake agent gained `SANDBOX_FAKE_BRANCH` (an unpushed branch in `$MULTICA_WORKSPACES_ROOT/.repos/sandbox-fake`); the issue is reassigned from agent A to agent B. Across pods runs with hold 20s and `SANDBOX_POD_MAX_IDLE=10s`, so A's pod is gone before B. There is no git remote in the test, so "only what A pushed" is shown as: B finds no branch and no file of A's.
- Clean resume: after the clean pod is deleted, a comment on the issue queues a task that Multica sends as a resume; it runs on a new pod and completes.
- Budget: 3 pods, 1 warm; b holds one pod (guarantee 1), a holds one and has one idle; b's new key deletes a's idle pod. Then b's guarantee goes to 2 and a's new key deletes a's own held pod, never b's.
- Cannot start: a template with a missing image and a 20s start timeout; the task stays queued 50s and the inventory's `problem` names `ErrImageNeverPull`.
- Restart: a 45s task, the manager stopped and started mid-task.
- Privacy: every pod name and IP seen (polled every second) is searched for in the runtimes, issues, comments, task runs and task messages of both workspaces.

**Results** (kind `sandbox-dev`, dev-user kubeconfig, fake image rebuilt with the new fake agent): runs 5, 6 and 7 passed all 8 scenarios back to back, 240s each; no managed pod left after any run. `smoke.sh` passes in 13s. The run's own manager uses ports 8796/8797.

**Two manager bugs found and fixed (TDD):**
- **A probe deleted a held pod.** When the budget was full, a claim probe of a pending list with nothing queued (one per new list) went through `needPod` and deleted a held pod, then claimed nothing. Probes now only ask for a pod (`policy.probing`; test "a probe of a list with nothing queued never deletes a pod").
- **`SANDBOX_WARM_PODS=0` deadlocked** with no pods: runtime IDs are learnt from a pod's daemon registering, so the pending lists were never read and nothing asked for a pod. `0` is now refused at start (`TestLoadSettingsRefusesNoWarmPod`); the docs say at least 1.

**devpool:** `deploy/devpool/run.sh` ran against the kind Multica (with `DEVPOOL_STATE` and `-admin 127.0.0.1:8798`, beside the older devpool still on 8791) and showed warm and held simulated pods and budget deletions; it now takes `DEV_STATE`. Its dispatch page shows `max: 0` (the simulated pool has no budget of its own), and its banner always names port 8791; cosmetic, left.

**Also checked:** `deploy/k8s/manager.yaml` with `rbac.yaml` applied in the kind namespace (manager image `multica-sandbox-manager:e2e` loaded into kind): the in-cluster manager ran a fake task to completion on the service account; everything was deleted afterwards. The first two pods it created exited with code 1 within a second of the manager starting (likely the Service not reachable yet) and were replaced.

**Open:** the scenarios run the manager on the operator's host, so pods must reach that host (`E2E_MANAGER_URL`); against the company's namespace that may need running them from inside the network. A run leaves its bot account and PAT in Multica.
