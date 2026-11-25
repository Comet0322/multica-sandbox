# 03: Pod image without the supervisor

**What to build:** The pod image runs `tini` and the stock daemon, configured from the environment the sandbox service sets. An example pod template lives in `deploy/k8s/`.

- **Image:** the entrypoint writes the daemon's config from the pod token and the manager's URL, then execs the daemon under `tini`. The settings the supervisor applied move to the entrypoint or the pod's `securityContext`: the private umask, the read-only root and the daemon's home.
- **Example template:** requests and limits, a liveness probe on the daemon's health endpoint, a neutral fixed hostname, and no secrets in the environment beyond the pod token.
- **RBAC example:** `deploy/k8s/` also carries the YAML for a dedicated service account, with a Role allowing only `create`, `delete`, `get`, `list` and `watch` on pods, and its RoleBinding, for when the operator stops using a personal kubeconfig.

**Blocked by:** 02

**Status:** resolved

- [x] A pod built from the example template registers, runs a task with the stock v0.6.4 daemon, and reports through the pool side
- [x] The template passes the test namespace's admission policies, and any change needed is noted in the template
- [x] No pod name, hostname or IP reaches Multica (checked on the runtime list and in a task's messages)
- [x] The RBAC YAML applies cleanly with `kubectl apply --dry-run=server` where the operator has the rights, or is reviewed if not

## Comments

Partly done in issue 02:
- the image has `tini` and `deploy/pool/entrypoint-kube.sh`;
- `deploy/k8s/dev/pod.yaml` is a template that passes Pod Security `restricted`: non-root, read-only root, all capabilities dropped, RuntimeDefault seccomp, and an exec liveness probe on the daemon's loopback health endpoint.

Still to do:
- **A neutral hostname.** The fake agent's comment shows the pod name as the hostname, and real agents may print it too. Set `spec.hostname` in the template.
- The production template and the RBAC example under `deploy/k8s/`.
- A real agent image's memory numbers.

Done in issue 03 (branch `ticket/03-pod-image`, 2026-11-16):
- **Neutral hostname.** `KubeService` sets `spec.hostname: sandbox` when the template has none and keeps the template's own (`TestKubeGivesPodsANeutralHostname`; glossary "Pod template" updated). Both templates set it too.
- **`deploy/k8s/pod.yaml`**: the example template for the company: `tini` and the stock daemon, requests 500m/1Gi, limit 4Gi, ephemeral storage, the exec liveness probe on `127.0.0.1:19514/health`, hostname `sandbox`, restricted securityContext, no service account token, and only `SANDBOX_MANAGER_URL` in the env beside the pod token. Image and manager address are placeholders. The umask and daemon home stay in `entrypoint-kube.sh`; the read-only root is in the securityContext.
- **`deploy/k8s/rbac.yaml`**: ServiceAccount `sandbox-manager`, Role `sandbox-manager-pods` (pods: create, delete, get, list, watch) and its RoleBinding. `kubectl apply --dry-run=server` passes with the kind admin context; the pods-only dev-user is refused, as an operator without RBAC rights would be. Applied briefly and checked: `auth can-i` gives yes for the five verbs and no for update, patch and Secrets; then deleted. (`auth can-i create pods/exec` without `--subresource` wrongly says yes; real `exec` and `logs` with dev-user are refused.)
- `deploy/k8s/README.md` describes the pod, its memory and the RBAC example.

**Verified in kind** (`sandbox-dev`, dev-user kubeconfig, manager from `deploy/k8s/dev/manager.sh` with its state copied to a scratch dir):
- Both templates pass `kubectl create --dry-run=server` with no `restricted` warnings (the namespace enforces baseline and warns on restricted). Kyverno, Gatekeeper and Istio are not installed, so not checked; the template notes what Istio would need.
- A fake-agent task ran on a manager-created pod and completed; its comment reads "agent ran in sandbox". The pod names and IPs seen were absent from the runtime list, the task runs (result) and the issue's comments. The task's message list was empty for the fake agent, so messages were checked only trivially.
- **Real agent image**: the `agents` target built (npm from the internet worked; Claude Code 2.1.325, opencode 1.19.5) and was loaded into kind. A pod on it registered (runtime device "Managed Sandbox · 2.1.325 (Claude Code)") and ran a real Claude Code task with no model reachable (bogus `ANTHROPIC_BASE_URL`): idle pod 10 MiB (90 MiB peak while the daemon probes CLIs at start), pod peak about 145 MiB during the task, claude RSS about 200 MiB. Each attempt failed after about 3 minutes and Multica retried it on a new pod; I cancelled and archived the test agents. opencode starting a task with no model peaked at about 1.2 GiB in a plain container.

**Not done / open:**
- The tasks ran on pods from `deploy/k8s/dev/pod.yaml` (same shape, local image and host manager address), not from `deploy/k8s/pod.yaml` itself, whose image and manager address are placeholders; that one was only dry-run.
- A real task's peak with a model (no credentials here), so requests and limits in `pod.yaml` are an estimate.
- Liveness failure shows as "the pod's daemon container exited with code 137 (Error)", not a dedicated "daemon not answering" reason.
- The image's default ENTRYPOINT is still the supervisor's, for the GitOps path until issue 07; the template sets the command.
- No manager Deployment for the in-cluster service account yet.
- Two orphaned `sandbox-*` pods from an earlier run were in the namespace when I started; my manager deleted them as leftovers. No pods or manager are left running.
