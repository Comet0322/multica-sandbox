Type: research
Status: open

# Pods reaching the manager

## Proposal

- The pod template sets the manager URL to the Service's fully qualified name, `http://sandbox-manager.<manager-ns>.svc.cluster.local:<service port>` (8080 in our manifests). Istio blocks cross-namespace traffic by default but lets a host addressed by this full name through (operator verified, 2026-11-26); short names such as `sandbox-manager.<ns>.svc` are not used. Traffic stays inside the mesh, no ingress route.
- `deploy/k8s/pod.yaml` sets `SANDBOX_MANAGER_URL` to the short name `http://sandbox-manager:8080` today, which only works inside the manager's namespace. Change it to the full name in delivery step 4, and say in `docs/DEPLOYMENT.md` that the short name does not cross namespaces under Istio.
- No NetworkPolicy: pods can reach everything in the cluster the mesh allows. Every endpoint a pod can reach checks a credential: the daemon protocol the pod token, the sandbox service its bearer secret (04). The admin listener stays on 127.0.0.1.
- Failover gap: the daemons ride it out on their own (01). What a gap costs is the transcript messages sent during it.

To verify in kind: a pod in a second namespace registers through the Service name, and a running task survives killing the leader.
