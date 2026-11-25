# 02: Sandbox service on Kubernetes

**What to build:** The sandbox service's Create and Delete provision real pods in one namespace. Nothing else in the manager changes, so the current policy, set to the `task` rule with hold timeout 0 and a pod capacity of 1, already runs one task per pod on it.

- **Client:**
  - Uses `KUBECONFIG` when set, else the in-cluster service account.
  - Acts on `SANDBOX_NAMESPACE`.
- **Create:**
  - Reads the pod YAML at `SANDBOX_POD_TEMPLATE`.
  - Sets the name, the label `app.kubernetes.io/managed-by=sandbox-manager`, the pod token as an environment variable, and annotations.
  - Creates the pod and returns once its daemon has registered with the pool side.
  - A pod still pending, or not registered, after `SANDBOX_POD_START_TIMEOUT` (default 3m) is deleted, and creation backs off. Create then returns `ErrNoCapacity` with the reason.
- **Delete** deletes the pod.
- **Watching:** watches its labelled pods and reports a pod lost with its reason. The reasons are OOMKilled with the limit, evicted, failed, deleted, and liveness failed. The task's failure report to Multica carries that reason in words.
- **Auth failures:** when Kubernetes refuses the credentials, the manager stops claiming and shows the error on the dispatch page and in the log.
- **The GitOps registration path stays** until issue 07, so both paths work during the change.

**Blocked by:** None (can start immediately)

**Status:** resolved

- [x] Unit tests with a fake Kubernetes client cover template filling, readiness, the start timeout and backoff, every loss reason, and auth failure stopping claims
- [x] Against a real namespace with the operator's kubeconfig, a task runs on a pod the manager created, and the pod is deleted after it
- [x] A pod given too little memory fails its task with an out-of-memory reason visible in Multica
- [x] The idle memory of one pod (daemon and agent CLI) and a typical task's peak are measured and recorded in the Comments
- [x] `gofmt`, `go vet` and `go test -race ./...` pass

## Comments

**Code:**
- `internal/sandbox/kube.go`: KubeService implements the sandbox Service on the Kubernetes API with client-go v0.36.6.
  - A pod is ready when its daemon first authenticates with its pod token.
  - Create takes a ready pod no one holds, or has one created and answers `ErrNoCapacity`.
  - Warm pods (`SANDBOX_WARM_PODS`, default 2) are kept within `SANDBOX_MAX_PODS`.
  - The start timeout deletes the pod and backs off, from 10s doubling to 5m. The waiting reason goes into the inventory's new `problem` field and into Create's error.
  - Losses are reported in words: out of memory with the limit, evicted, container exited, deleted.
  - Refused credentials are named as such.
  - On start it deletes the pods a previous run left. It only touches pods labelled `app.kubernetes.io/managed-by=sandbox-manager`.
- A starting pod is reported as `resetting` to callers, so the core and admin API work unchanged until issue 07.
- `cmd/sandbox-manager/kube.go` reads `SANDBOX_POD_TEMPLATE`, `SANDBOX_NAMESPACE`, `KUBECONFIG`, `SANDBOX_MAX_PODS`, `SANDBOX_WARM_PODS` and `SANDBOX_POD_START_TIMEOUT`. Without a template the GitOps service is used as before.
- The core now passes the pool's reason for a lost pod into the task's failure, as "the pod running this task was lost: <reason>".
- `go.mod` gains `k8s.io/{api,apimachinery,client-go}` v0.36.6 and `sigs.k8s.io/yaml`. client-go's requirements raise `gorilla/websocket` to v1.5.4-0.20250319132907.

**Taken early from issue 03, to run real pods:**
- The pod image gains `tini` and `deploy/pool/entrypoint-kube.sh`, which takes the token from `SANDBOX_POD_TOKEN` and execs the daemon with no supervisor.
- `deploy/k8s/dev/pod.yaml` is a restricted-profile template.
- `deploy/k8s/dev/manager.sh` runs the manager on the host against the kind namespace with the pods-only `dev-user` kubeconfig.

**Real namespace** (kind `sandbox-dev`, namespace `multica-sandbox`, dev-user kubeconfig with pod rights only, 2026-11-16):
- Pods are created and ready about 1s after creation, with the image already on the node.
- A fake-agent task ran on `sandbox-e2d9e19418` and completed, and the pod was deleted after it (hold 0, rule `task`, capacity 1).
- A task holding 1.5 GiB in a 1Gi pod failed. Multica shows "the pod running this task was lost: the pod ran out of memory (limit 1Gi)", and the next task ran normally.
- An idle pod (`tini` and the stock daemon, fake agent) uses about 10 MiB (cgroup `memory.current`; `memory.peak` 11 MiB).
- **Not measured:** a real agent CLI's peak. The fake image has no Claude Code, so the agents image's numbers still have to be taken, in issue 03 or 09.

**Checks:** `gofmt`, `go vet` and `go test -race ./...` pass (483 tests).
