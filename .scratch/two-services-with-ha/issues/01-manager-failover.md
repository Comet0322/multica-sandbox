Type: grilling
Status: open

# Manager failover

The manager is the one daemon Multica sees; two active task sources would claim twice. 

## What Multica v0.6.4 tolerates (read from the v0.6.4 source, 2026-11-26)

- A runtime goes offline 150 to 180 s after its last heartbeat; its tasks are failed only after a 3 h reconnect grace (`runtime_sweeper.go:26,37-43`).
- A task dispatched and never started is handed again to the next claim from the same runtime after 90 s (`ReclaimStaleDispatchedTaskForRuntime`, `task.go:193-197`) and failed as `timeout` (retryable) after 300 s.
- Claims carry no idempotency key, but `dispatched_at` fences StartTask: an outdated delivery gets 409 and the daemon drops it silently (`agent.sql:964-970`, `daemon.go:5800-5804`).
- The pod's daemon retries heartbeats and claims forever, keeps complete/fail reports in an on-disk outbox and replays them for hours (`terminal_report_queue.go`). Progress and transcript messages are not retried: those sent during a gap are lost.
- The pod's daemon kills its agent only on 404 or a terminal status for its task; other errors (the manager answers 403 for an unknown task) are ignored. Three 403/404 on a completion report spread over 10 minutes drop it.
- recover-orphans fails every dispatched or running task of a runtime at once (`agent.sql:1196-1211`).

So a failover under about 60 s costs nothing upstream, and the only thing lost is the pods' transcript messages during the gap.

## Proposal

Two replicas, one leader, elected in Postgres.

- Table `leader(name, holder, generation, expires_at)`. A replica takes or renews it with one `UPDATE ... WHERE expires_at < now() OR holder = $me RETURNING generation`, on Postgres's clock. TTL 15 s, renew every 2 s; a leader that has not renewed within 10 s stops all upstream and pod work on its own clock before the TTL lets anyone else in (client-go's leaseDuration / renewDeadline / retryPeriod split).
- Every write to the dispatch tables (02) checks the generation in the same statement. A deposed leader's writes fail, and it exits.
- Only the leader is Ready (`/readyz`), so the Kubernetes Service sends the pods' daemons and Multica relays to it alone. A follower connects to Postgres and does nothing else.
- Taking over: load the state from Postgres (02), take the sandbox service's snapshot and reconcile (03), then become Ready and start the dispatch tick. Never Ready before the state is loaded, so no pod ever gets a 404 for a live task.
- On SIGTERM the leader stops work, sets `expires_at = now()` and exits, so a rolling update fails over in about one renew period.
- Orphans: the new leader does not call recover-orphans. A task the old leader claimed but never recorded is handed again by Multica after 90 s to the new leader's next claim on that runtime, or failed as a retryable timeout at 300 s. `recoverOrphansOnce` (tasksource/source.go) and the RecoverOrphans call in `Runtimes` registration (runtimes.go:208) go, since the database always knows what the manager holds.

Measured target: failover gap from leader kill to the new leader Ready, under 20 s; prove it in the e2e scenarios with a task running across the kill.

Rejected: Postgres session advisory lock (the lock dies with one connection but writes on the other pooled connections still go through, so it fences nothing); a Kubernetes Lease (no RBAC); active-active (pods' connections land on any replica).

## Under Multica v0.11.0 (multica-v0.11-compat-report.md, 2026-11-26)

All six facts above hold at v0.11.0. Two requirements follow from v0.11.0's runtime ownership checks: both replicas use the same bot account's PAT, and the same daemon ID, so `SANDBOX_MANAGER_DAEMON_ID` is set explicitly (otherwise it is derived from the exact URL string). A different owner makes Multica answer 404 "task not found", which makes the pods' daemons kill their agents.

Optional later: task messages accept an `idempotency_key` at v0.11.0 (`handler/daemon.go:5885-5886`). The manager could buffer pod messages across a failover and replay them with a key, closing the transcript loss accepted above.
