# 06: Rebuild state from pods on restart

**What to build:** The manager survives its own restart without failing running tasks. Each pod's annotations hold what the core needs: department, kind, key, current task, and hold start or idle start.

- The sandbox service writes the annotations when a pod is placed, held or made idle.
- On start, the manager lists its labelled pods and rebuilds the dispatch record and the policy's view from them:
  - running tasks run on and report through the pool side;
  - holds resume with their remaining time;
  - warm and idle pods are kept;
  - pods it cannot interpret are deleted.
- A task Multica reports as dispatched to the manager whose pod is gone is failed with a reason.

**Blocked by:** 05

**Status:** resolved

- [x] A test restarts the manager with a running task, a held pod, an idle pod and a warm pod, and each continues as before
- [x] Against the test namespace, restarting the manager mid-task does not fail the task
- [x] `gofmt`, `go vet` and `go test -race ./...` pass

## Comments

Branch `ticket/06-07`, commit `5dee6cc`.

**Decisions:**
- **Writing records needs `patch` on pods.** The spec's Role (create, delete, get, list, watch) cannot change a pod after creation, and a warm pod's department is only known at its first task. `deploy/k8s/rbac.yaml` and `deploy/k8s/dev/dev-user.yaml` now allow `patch`; I applied the dev-user Role to the kind cluster. Whether the operator's own kubeconfig at the company may patch pods is **not known**: the spec's "Authentication" section and ADR 0009 still list five verbs and should gain `patch`. Without it the manager runs, shows the refusal on the dispatch page, and a restart deletes every pod (Restore probes with a dry-run patch), since a pod with no record could have served anyone.
- **Where it lives:** labels `sandbox-manager/workspace` and `sandbox-manager/kind`; annotations `sandbox-manager/{phase,key,task,task-started,since,until}`. A warm pod carries none. The core sends a record whenever a held pod's policy view or lease changes (`core.Pool.Record`, non-blocking); `KubeService` patches the latest record per pod in the background with a JSON merge patch.
- **Start order:** `Manager.Restore` runs before the first Sync (`cmd/sandbox-manager/run.go`), replacing `deleteLeftovers`. Restored serving pods are held by the core; unrecorded pods are kept as starting and become warm when their daemon calls in (their start timeout counts from the restore). A recorded pod that ended meanwhile (OOM, eviction) is reported lost by the watch, so its task fails with the pod's reason.
- **Registrations:** a restored pod's daemon registered with the previous run. A heartbeat for a runtime the manager does not know, or of a workspace the pod has not registered for in this run, now answers `404 runtime not found`, which makes the stock daemon register again (its recover-orphans call stays intercepted).
- **Lost tasks:** a task placed on a pod that had not taken it up is failed ("the sandbox manager restarted before the task's pod took it up") and its pod deleted. On the first Sync, a workspace with restored work no longer skips orphan recovery: the tasks Multica shows *dispatched* to the manager's runtimes that the core does not hold are failed ("…restarted before the task reached a pod"). Workspaces with no restored work keep the blanket recover-orphans.
- Initiators are not recorded on pods (no personal data in Kubernetes), so a restored lease has no initiator in the audit trail.

**Verified in kind** (`sandbox-dev`, dev-user kubeconfig with `patch`, manager from `deploy/k8s/dev/manager.sh` with state copied to a scratch dir, fake image, `SANDBOX_RUNTIMES=pool`): an agent with `SANDBOX_FAKE_SLEEP=90` ran DEV-7; the pod carried `phase=running` and the task. I stopped the manager mid-task and started a new one: "pods restored serving=1 kept=3 deleted=0", the warm pods registered again after the heartbeat answer, the task completed in Multica (task run `completed`, comment posted), the lease ended "pod reported complete" and the pod's record became `held`. A third start restored the pod as held. Manager stopped and the three pods deleted afterwards. The test agent `slow-claude` is left in the dev workspace.

**Not done / open:**
- A running task whose pod was deleted *while the manager was down* (so it is not listed) is not failed by the manager: Multica shows it `running`, not `dispatched`, and recover-orphans would fail the restored tasks too. It waits for Multica's own timeout.
- The company's rights to patch pods (above).
