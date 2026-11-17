# Kubernetes manifests

Multica, the manager and its pods share one namespace. How to deploy them, the
pod template, the permissions and every setting are in the
[deployment guide](../../docs/DEPLOYMENT.md).

| Path | What |
| --- | --- |
| `multica/` | Multica v0.6.4 (Postgres, the backend, the web app), with no namespace in the manifests: `kubectl apply -k deploy/k8s/multica -n <namespace>` after creating the `multica-backend` Secret and ConfigMap. |
| `pod.yaml` | The example pod template (`SANDBOX_POD_TEMPLATE`). Set its image and size it before use. |
| `rbac.yaml` | The manager's service account, with a Role that may only `create`, `delete`, `get`, `list`, `watch` and `patch` pods and `get` its own Deployment (for `SANDBOX_POD_OWNER`), and its RoleBinding. |
| `manager.yaml` | An example Deployment and Service for the manager, on that service account or a mounted kubeconfig. |
| `dev/` | A local kind cluster with one namespace standing in for yours (`up.sh`, `manager.sh`, `down.sh`). |
| `e2e/` | End-to-end scenarios against a namespace with real pods (see [e2e/README.md](e2e/README.md)). |
