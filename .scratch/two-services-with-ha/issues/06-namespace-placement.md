Type: grilling
Status: open
Blocked by: 03

# Which namespace a new pod goes to

The sandbox service owns this; the manager sees only "created in ns X" or "no capacity, because ...".

## Proposal

Configuration, one entry per namespace, in order of preference:

- namespace name;
- soft cap on our pods there (the namespace's ResourceQuota is the hard limit);
- pod template (default: the shared one);
- owner (07).

Placement on `PUT /v1/pods/{name}`:

1. Skip namespaces that are unhealthy or at their soft cap, counted from the watch cache.
2. Take the first remaining one in configured order (fill in order; keeps the spread predictable and the other namespaces' quota free for others).
3. If the Kubernetes create is refused for capacity (quota exceeded, forbidden), mark that namespace and try the next one in the same call.
4. None left: 503 with each namespace's reason.

Health per namespace, today's `startFailures` / `broken` logic (5f0079a) kept per namespace:

- a permanent failure (image pull denied, admission refused, owner missing) or `StartFailures` start failures in a row marks it broken;
- while broken it gets one probe pod at a time; the first pod that runs clears it;
- the state lives in each replica's memory and is rebuilt from what that replica observes. It is soft: a fresh replica starts healthy and relearns from one failed pod, and two replicas may each send one probe. Both cost at most a few extra failed pods, never a task (no task is claimed before a pod is ready, ADR 0009).

Start timeout: the sandbox service reports a pod that did not start in time as failed with its classified reason; the manager deletes it, as it deletes every pod.

The manager's budget stays global. Per-namespace caps never reach the policy, so `internal/policy` keeps knowing no Kubernetes.

Prerequisites each namespace must have, checked at startup and reported as that namespace's health: RoleBinding for our ServiceAccount, imagePullSecret named by the template, the owner object (07).
