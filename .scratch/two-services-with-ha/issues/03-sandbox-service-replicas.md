Type: grilling
Status: open

# Several sandbox service replicas

Requirement: the sandbox pods survive any replica's death and the remaining replicas keep managing all of them (watch, lost detection, Delete) with no gap the manager notices. The pods need no HA of their own.

## Why today's service cannot simply run twice

`KubeService` (internal/sandbox/kube.go) keeps decisions in process memory under one lock:

- which pods are free and which are held (`pods`, `PodState`);
- the warm count, the budget (`MaxPods`), backoff and `broken`, so it decides when to create;
- pod tokens (`tickets`) and readiness, set by the first `Authenticate` from the manager's daemon handler;
- the core's record on each pod (`Record`, `Restore`, annotations in record.go).

Two replicas would each top up warm pods (overshooting the budget), could hand one free pod to two callers, and would disagree on readiness, which only the replica the manager happened to call has seen.

## Proposal: decisions in the manager, the sandbox service only carries them out

The manager is one leader with Postgres (01, 02). Every decision moves there, and the sandbox service keeps only what needs Kubernetes knowledge or RBAC. Then the replicas share nothing but Kubernetes, and need no lock, lease or compare-and-swap.

Moves to the manager:

- warm count and the overall budget (already policy decisions per the glossary);
- free / held / what each pod serves: rows in Postgres, replacing `Record` / `Restore` on annotations;
- pod tokens: the manager generates one, stores its hash, and passes it to Create; it authenticates daemons itself, and the first daemon call is readiness, which the manager already sees.

Stays in the sandbox service:

- the pod template per namespace and rendering it (name, labels, token env, owner);
- create and delete through the Kubernetes API, in any namespace it is bound to;
- watching pods and classifying failures (failure.go);
- namespaces: which one a new pod goes to, each one's health and soft cap (06), each one's owner (07);
- the audit of Kubernetes actions.

API, every call idempotent:

- `PUT /v1/pods/{name}` with the token. The manager picks the name and records the intent in Postgres before calling. The sandbox service picks the namespace and returns it; the manager stores it. A name already present in any namespace is returned as is, so a retry after a timeout or a replica death is safe. When no namespace can take a pod: 503 with each namespace's reason, which the manager treats as today's `ErrNoCapacity`.
- `DELETE /v1/namespaces/{ns}/pods/{name}`, with the namespace the manager stored. NotFound is success.
- `GET /v1/pods?watch=1`: a snapshot of every managed pod with its status (pending, running, failed with classified reason, gone), then changes. Any replica serves it. When the stream breaks the manager reconnects to the Service, gets a fresh snapshot and reconciles against Postgres: a row with no pod is lost, a pod with no live row is deleted. Rows are keyed by namespace and name, so a duplicate a racing retry left in a second namespace is deleted too.
- Bearer credential that only the manager holds. Without NetworkPolicy the sandbox pods can reach this API.

Decided:

- Instance label. The sandbox service is configured with an instance id and puts `sandbox-manager/instance=<id>` on every pod it creates. Its watch, its placement counts and the manager's reconcile see only pods with its own id, so two deployments (dev, prod) sharing a namespace never adopt or delete each other's pods.

Open:

- Namespace placement and health: proposal in 06.
- Owner references per namespace: proposal in 07.

## Alternative kept for comparison

Keep decisions in the sandbox service and run replicas active-active: acquire by patching a pod annotation with its `resourceVersion` as precondition (409 means another replica won), tokens read back from pod env on every replica, readiness written to an annotation. Rejected for now: the warm top-up and the budget still need one counter across replicas, readiness becomes a cross-process write per daemon call, and the manager gets a second store of the same facts it keeps in Postgres.
