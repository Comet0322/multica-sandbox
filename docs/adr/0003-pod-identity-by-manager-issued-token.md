# Pods are identified by a manager-issued token, not by source IP

The manager used to identify a pod by the source IP of its requests. The company's Istio mesh proxies pod traffic and rewrites that address, so every pod would look the same.

Instead, the pod's entrypoint calls a registration endpoint on the manager before anything else starts. It writes the returned pod token into the daemon's `.multica/config.json` and then starts the supervisor, which starts the daemon. The daemon and the supervisor's control channel present this token on every call. The manager assigns each pod its identity; a Kubernetes pod name is only a display label. The entrypoint runs only when the container starts, so a token lasts until the container stops; resets keep it, because a reset never deletes the files created at container start.

## Consequences

- The registration endpoint is open for now: anything that can reach the manager's URL can register as a pod and receive tasks, including each agent's custom environment.
- Agent `multica` CLI calls carry the task's `mat_` token rather than the pod token, so the manager forwards them without knowing which pod sent them.
