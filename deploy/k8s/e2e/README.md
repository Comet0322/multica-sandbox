# End-to-end scenarios

`scenarios.sh` and `smoke.sh` run the manager on this host against a namespace
that holds a Multica v0.6.4 backend, and let it create real pods there from
the stock v0.6.4 daemon in the fake-agent pod image. Nothing is mocked: the
pods, the daemon, the backend and the Kubernetes API are real.

| Script | What it checks | Time |
| --- | --- | --- |
| `smoke.sh` | One pool task and one clean task complete on real pods; no pod detail reaches Multica | under a minute |
| `scenarios.sh` | Every scenario below | about 4 minutes |

The scenarios, in the order they run:

| Scenario | What passes |
| --- | --- |
| Handoff across pods | Agent A commits to an unpushed branch. After A's hold and its idle pod are gone, agent B, handed the same issue, lands on another pod and finds neither the branch nor any of A's files: only what A pushed would reach it. |
| Clean | A clean task gets a pod that served no one. The same issue's next task gets that pod again while it is held; another issue's task does not. After the hold, the pod is deleted, and the issue's next task (a resume, for Multica) runs on a new pod and completes. |
| Out of memory | A task holding 1.5 GiB in a 1 GiB pod fails with `the pod ran out of memory (limit 1Gi)`, and the next task completes. |
| Restart | The manager is stopped while a task runs and started again; the task completes. |
| Handoff on one pod | Agent B, handed agent A's issue while A's pod is held, lands on that pod and finds A's unpushed branch. |
| Budget | With 3 pods all in use, a task of department b deletes department a's idle pod, not a's or b's held pods. With b's guarantee raised to 2, a task of a deletes a's own held pod, never one of b's. |
| Pod that cannot start | With a template whose image does not exist, a task stays queued in Multica, and the manager shows why pods do not start. |
| Privacy | No pod name or pod IP seen during the run appears in Multica's runtimes, issues, comments, task runs or task messages, and agents see the hostname `sandbox`. |

The fake agent (`deploy/fake/fake-claude`) stands in for a real CLI: it
comments on its issue with the hostname, the files earlier tasks left on the
pod, and, with `SANDBOX_FAKE_BRANCH`, whether a branch is in the pod's repo
cache. `SANDBOX_FAKE_MEMORY` and `SANDBOX_FAKE_SLEEP` make it use memory or
take time.

## Running against the local kind namespace

After `deploy/k8s/dev/up.sh` and with `multica-sandbox-pool:kube-fake` built
and loaded (see the [deployment guide](../../../docs/DEPLOYMENT.md#local-kind-namespace)):

```bash
deploy/k8s/e2e/smoke.sh
deploy/k8s/e2e/scenarios.sh
```

## Running against another namespace

The scripts read these settings from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `E2E_KUBECONFIG` | `deploy/k8s/dev/.state/dev-user.kubeconfig` | The kubeconfig the manager creates pods with. It needs `create`, `delete`, `get`, `list`, `watch` and `patch` on pods, nothing else. |
| `E2E_NAMESPACE` | the kubeconfig context's | The namespace the pods are created in. |
| `E2E_API` | `http://127.0.0.1:18080` | The Multica backend, as this host reaches it. |
| `E2E_CODE` | from `deploy/k8s/dev/.state/backend.env` | The backend's `MULTICA_DEV_VERIFICATION_CODE`: the scripts sign in a bot account of their own with it, so the backend must be a test backend. |
| `E2E_EMAIL_DOMAIN` | `dev.test` | Domain of the bot account's address (`e2e-<run>@<domain>`). |
| `E2E_MANAGER_URL` | the kind network's gateway, port `E2E_POD_PORT` | How the pods reach the manager on this host. |
| `E2E_POD_PORT`, `E2E_ADMIN_PORT` | `8796`, `8797` | The manager's pod listener (all interfaces) and admin API (loopback). |
| `E2E_POD_IMAGE` | `multica-sandbox-pool:kube-fake` | The fake-agent pod image, as the namespace pulls it. |
| `E2E_PULL_POLICY` | `Never` | Its pull policy; `IfNotPresent` for an image in a registry. |
| `E2E_DEV_STATE` | `deploy/k8s/dev/.state` | Where the kind namespace's state is, for the defaults above. |
| `E2E_STATE` | `$TMPDIR/sandbox-e2e-<run>` | The run's own files: manager log, audit log, pod template, overrides. |
| `KEEP` | unset | `1` keeps the run's workspaces in Multica for inspection. |

For example:

```bash
E2E_KUBECONFIG=~/.kube/config E2E_NAMESPACE=<namespace> \
E2E_API=https://<test backend> E2E_CODE=<code> \
E2E_MANAGER_URL=http://<this host, as pods reach it>:8796 \
E2E_POD_IMAGE=<registry>/multica-sandbox-pool:<tag>-fake E2E_PULL_POLICY=IfNotPresent \
deploy/k8s/e2e/scenarios.sh
```

The pods must be able to reach this host at `E2E_MANAGER_URL`. Where they
cannot, run the scripts from a machine they can reach.

The scripts refuse to start when the namespace already holds pods labelled
`app.kubernetes.io/managed-by=sandbox-manager`: those belong to another
manager, which a second one would take over. Do not run them in a namespace
where a manager serves real users.

## What a run leaves behind

Each run signs in a bot account of its own and creates its own workspaces, so
it serves nothing else. On exit, passed or failed, it stops its manager,
deletes every pod labelled `app.kubernetes.io/managed-by=sandbox-manager` in
the namespace and waits until they are gone, archives the agents it created
and deletes its workspaces (kept with `KEEP=1`). The bot account and its
token stay in Multica. The manager log stays in `E2E_STATE`.
