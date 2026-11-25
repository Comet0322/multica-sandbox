Type: grilling
Status: open
Blocked by: 03

# Manager and sandbox service API

The calls are in 03. What remains:

- Transport: HTTP/JSON on the standard library, the same stack as the daemon protocol. The watch is NDJSON over a chunked response: one snapshot line per pod plus one line of namespace health, then a line per change, and a heartbeat line every 10 s so either side notices a dead stream.
- Authentication: one bearer secret from a Kubernetes Secret, mounted in both Deployments, compared in constant time. Without NetworkPolicy the sandbox pods can reach the sandbox service, so every call except `/healthz` requires it.
- Audit: the sandbox service puts the classified failure and the Kubernetes action on the watch lines; the manager writes the one audit log, as today.
- Inventory and dispatch page: the manager merges its database with the latest snapshot and namespace health. Namespace health is per sandbox replica and soft (06); the page shows the one the manager's stream comes from.
- Lost pods: a pod that turns failed or gone on the stream is lost; the manager fails its task and deletes it, as `Notifier.Lost` does now.
- Failover of the sandbox service: the stream breaks, the manager reconnects through the Service to another replica, takes the new snapshot and reconciles. A create in flight is retried with the same name (03).
