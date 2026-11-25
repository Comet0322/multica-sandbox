Type: grilling
Status: open

# Monitoring the manager and the sandbox service

## Today

No metrics. What exists: slog logs, the audit log (JSON Lines), the admin API and dispatch page on 127.0.0.1:8791, `/healthz` and `/readyz` (readyz: a workspace listing succeeded within three sync intervals). Nobody is alerted when pods stop starting, the PAT is revoked or tasks pile up; someone has to open the dispatch page.

## Proposal

A Prometheus `/metrics` endpoint on the manager (`github.com/prometheus/client_golang`), on its own listener, plus alerts written as a PrometheusRule or plain rules file, whichever the company's stack takes.

Manager:

- `sandbox_manager_leader` (1 on the leader, 0 on a follower), `..._leader_changes_total`, `..._lease_renew_failures_total`;
- `..._store_errors_total{op}`, `..._store_latency_seconds{op}`;
- `..._upstream_errors_total{call}` (heartbeat, pending, claim, fail, task status), `..._last_sync_timestamp_seconds`;
- `..._tasks{state}` (placed, handed, running), `..._tasks_claimed_total{tenant,kind}`, `..._tasks_failed_total{tenant,reason}`, `..._fail_reports_pending`;
- `..._queue_depth{tenant,kind}` (Multica pending seen at the last tick), `..._claim_wait_seconds` (queued upstream to handed);
- `..._pods{phase}` (warm, running, held, idle), budget used and max;
- `..._sandbox_stream_connected`, `..._sandbox_stream_reconnects_total`.

Sandbox service: no `/metrics` of its own for now (decided 2026-11-26). Everything it knows reaches the manager on the watch stream (03, 04), so the manager exposes it:

- `sandbox_manager_pod_failures_total{namespace,phase,kind}` from the classified failures;
- `..._pod_start_seconds{namespace}` (created to running, from the stream's timestamps);
- `..._namespace_healthy{namespace}`, `..._namespace_pods{namespace}`;
- `..._sandbox_api_errors_total{call}` (create, delete, watch), which also shows a sandbox replica that cannot reach Kubernetes, since the Service spreads calls over the replicas.

A sandbox replica that is down shows in Kubernetes (readiness, restarts). Add metrics to the sandbox service only if a question comes up that these cannot answer.

Alerts (first cut):

- no leader for 1 min, or leader changes more than 3 in 15 min;
- `last_sync` older than 3 sync intervals (PAT revoked, Multica down);
- a namespace unhealthy for 10 min, or every namespace unhealthy;
- `fail_reports_pending` above 0 for 10 min;
- tasks queued in Multica with no claim for 10 min while budget is free;
- store errors above 0 for 5 min.

Delivery: the leader metrics with step 2, the pod and namespace ones with step 3, the rest as a step 5 after step 4. Labels `tenant` and `namespace` are bounded (16 departments, a few namespaces); task IDs never become labels.

## To confirm with the operator

- Decided (operator, 2026-11-26): the operator configures Prometheus to scrape `/metrics`. We ship the endpoint, an example scrape config in docs/DEPLOYMENT.md (one job, by Service DNS or pod discovery on our labels), and the alerts as a plain Prometheus rules file under deploy/; no ServiceMonitor or PrometheusRule objects. Both manager replicas are scraped; every manager metric except `leader` is meaningful only on the leader, so alerts filter on `sandbox_manager_leader == 1`.
- Where alerts go.
- Whether logs are collected centrally (Loki, ELK); the audit log could go there instead of a file.
