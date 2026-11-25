# Manager and sandbox service as two services, each highly available

## Goal

Split the program into two deployments for production:

- **Manager** (task source + core + pool side toward the pods' daemons). Keeps the full dispatch record in Postgres so a restart or failover loses no task.
- **Sandbox service** (`internal/sandbox`, made its own process). Creates, watches and deletes pods in **several namespaces of the same cluster**, to use more quota.

Both services run with more than one replica. The sandbox pods themselves need no HA: a pod that fails loses only its own task (already handled). What must not happen is that losing one replica of either service loses or fails the sandboxes and tasks it was managing. Pods' lifetimes are independent of any service replica; another replica takes them over.

Today, with one replica, a restart already keeps the pods (they are owned by the Deployment, not the process) and `Core.Restore` takes them over from their annotations: running tasks run on. What a restart still costs: tasks placed on a pod but not yet taken up are failed (`reasonRestartedBeforeStart`), and nothing is dispatched or served to the daemons until the process is back. HA has to remove both.

## Notes

### Environment constraints (operator, 2026-11-26)

- No CRDs, no RuntimeClass, no cluster-wide controllers.
- RBAC per namespace: `pods` with `create, delete, get, list, watch, patch`. No `leases` assumed.
- Postgres is available to the manager.
- MinIO (S3-compatible object storage) is available; good to have, nothing in this effort depends on it.
- No NetworkPolicy and no Services beyond the static ones in our own manifests. Every API a sandbox pod can reach must authenticate its caller.
- All namespaces are in one cluster. Verified: a ServiceAccount in namespace A, bound by a RoleBinding in namespace B, manages pods in B.

### Prior art surveyed (2026-11-26)

- **kubernetes-sigs/agent-sandbox** (Go, Apache-2.0, v1.0.12). Sandbox / SandboxTemplate / SandboxWarmPool / SandboxClaim CRDs plus a controller. **Not usable here: needs CRDs and a controller.** Its "Anthropic Managed Agents" use case is the same shape as ours (dispatcher polls work, claims a warm pod, late-binds the session) and the late-binding idea carries over to plain pods.
- **OpenSandbox** (Python lifecycle server, Go operator + execd, Apache-2.0). BatchSandbox / Pool CRDs. **Not usable: CRDs.** Keeps allocation state in pod annotations, the same approach `internal/sandbox` already takes.
- **openma-ai/open-managed-agents** (TypeScript, Apache-2.0). Code not reusable. Its Node deployment scales horizontally with a SQL outbox (`managed_session_executions`): a renewable claim per session with owner, attempt and generation fence, FIFO within a session. **Reference design for the manager's dispatch table.**
- Conclusion: no project gives reusable code under these constraints. `internal/sandbox` already is the pods-only implementation and imports nothing else of this module, so it moves rather than being rewritten.

## Where it stands (2026-11-26)

- Multica v0.11.0 compatibility: done (`multica-v0.11-compat.md`).
- Step 1, the dispatch record in Postgres: done (`step-1.md`).
- Next: step 2, leader election with two manager replicas (issue 01 has the proposal; no step-2 brief yet).

## Decisions so far

- Multica `security/v0.11.0` fork only (the company's build); v0.6.4 support is dropped (multica-v0.11-compat.md).

- Pods reach the manager at `sandbox-manager.<manager-ns>.svc.cluster.local`; Istio allows cross-namespace traffic to the fully qualified name only (05).

- Pods carry `sandbox-manager/instance=<id>`; each deployment sees only its own (03).

- Sandbox service stays our own code (`internal/sandbox`), moved to its own process. No CRDs.
- Manager state goes to Postgres; this supersedes "Kubernetes holds the state, the manager keeps no database" (`internal/core/restore.go`).
- One sandbox service may manage pods in several namespaces through cross-namespace RoleBindings (verified).

## Fog

- How the manager fails over: active-passive with leader election in Postgres, or more. See 01.
- What the dispatch record holds and how a new leader resumes from it. See 02.
- Sandbox service replicas: proposal in 03. The manager keeps warm count, overall budget, held/free, tokens and readiness; the sandbox service is an idempotent create / delete / watch executor that also owns namespaces (placement, health, owner; 06, 07).
- The manager ↔ sandbox service API, including pod token checks and lost-pod notification. See 04.
- How the pods' daemons reach the manager across namespaces and through a failover. See 05.
- Which namespace a new pod goes to. See 06.
- Pod ownership across namespaces: an owner reference must be in the pod's namespace, so `SANDBOX_POD_OWNER=deployment/...` (commit 96b05ee) cannot point at a Deployment in another namespace. See 07.
- Out of this effort, worth its own: snapshot a task's workspace to MinIO keyed by issue and restore it on the next pod, so handoff no longer needs a held pod (agent-sandbox's suspend/resume without PVCs). Needs something in the pod to upload with a presigned URL; overlaps with agents pushing through the git MCP (ADR 0009).
- ADRs to supersede or add (0004, 0009, the no-database rule). After the above.

## Delivery order (proposal)

Beside the steps: Multica v0.11.0 compatibility on its own branch (`multica-v0.11-compat.md`). Once it lands, steps 2 to 4 verify against v0.11.0.

Each step ends with the e2e scenarios passing in kind.

1. Postgres store in the manager, one replica, sandbox service still in process: `Store` port, tables, takeover from the database. Pod annotations stop carrying the core's record. Check: restart the manager with a task placed but not started; it is handed again, not failed.
2. Leader election, two manager replicas. Check: kill the leader with a task running; it finishes and its report reaches Multica; gap under 20 s.
3. Sandbox service as its own process and Deployment, HTTP API, manager-issued tokens, one namespace. Check: kill a sandbox replica during a create and during a run.
4. Several namespaces: placement, health, per-namespace owner. Check: a namespace with a broken pull secret is skipped and reported.

ADRs once these are agreed: a new one for two services with a Postgres-backed leader, superseding ADR 0004's service shape (Create returning a ready pod, tokens in the sandbox service) and ADR 0009's "Kubernetes holds the state".

## Deliverables and acceptance (proposal, 2026-11-26)

Per delivery step: commits on a branch, `gofmt`, `go vet`, `go test -race ./...` green, the existing e2e scenarios green in kind, plus the new scenario of that step.

New e2e scenarios (deploy/k8s/e2e):

- step 1: restart the manager with a task placed but not started; it runs, not failed. Restart with a task running; it finishes.
- step 2: kill the leader (`kubectl delete pod --grace-period=0`) with a task running; the task finishes, its result reaches Multica, the gap from kill to the new leader Ready is measured and printed. Rolling restart of both replicas with tasks queued; none fails.
- step 3: kill a sandbox service replica during a create and during a run; no pod is duplicated or orphaned, no task fails.
- step 4: two namespaces; the first at its cap sends pods to the second; a namespace with a broken pull secret is skipped and shown on the dispatch page with its reason.

Not verifiable in kind, to check in the company cluster: Istio cross-namespace by full name, real ResourceQuota refusals, Argo CD showing pods under each namespace's owner, the company Postgres's own failover.

Cutover from today's single-replica manager: no import of the old pod annotations. Stop claiming (scale the old manager's Deployment to 0 once the dispatch page shows no running task), delete its pods by `app.kubernetes.io/managed-by` label, deploy the new version. Documented in docs/DEPLOYMENT.md.
