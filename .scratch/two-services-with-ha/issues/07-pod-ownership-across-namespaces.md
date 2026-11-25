Type: grilling
Status: open

# Pod ownership across namespaces

An owner reference must name an object in the pod's own namespace, so `SANDBOX_POD_OWNER=deployment/sandbox-manager` (96b05ee) cannot cover pods in other namespaces. 96b05ee wanted two things: Argo CD shows the pods under the app, and deleting the app removes them.

## Proposal

One owner object per namespace, shipped in the same per-namespace bundle as the Role and RoleBinding:

- a Deployment `sandbox-pods` with `replicas: 0` (it never runs anything; it only anchors the pods);
- the namespace entry in the sandbox service's configuration names it; the service reads its UID at startup (needs `get` on that one Deployment, as `rbac.yaml` already grants for the manager's);
- every pod created there gets it as a non-controller, non-blocking owner reference, as today.

Argo CD shows each namespace's pods under that Deployment, and pruning the app deletes the Deployment, and the garbage collector its pods.

A missing owner makes that namespace unhealthy with a permanent reason (06), instead of creating pods nobody cleans up.

Rejected: labels only, cleanup by the sandbox service. Argo CD would not show the pods, and uninstalling leaves them until someone deletes by label.
