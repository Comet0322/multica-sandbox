Status: open

# Rollout: stress tests, tuning, and whether it beats one runtime per department

Question from the operator (2026-11-26): once the manager is in the company, how to load-test it, how to tune it, and how to know it is better than the old setup of one fixed runtime per department (Multica workspace).

## What "better" means

ADR 0009 replaced fixed runtimes on promises. Each gets a number:

| Promise | Metric | Source |
| --- | --- | --- |
| A task that runs out of memory takes down no other task | Failed tasks that ended within 60 s of another failure on the same runtime; `runtime_recovery` failures. Near zero on the managed arm | `metrics.sh` 04 |
| Members do not wait longer | Ready to started, p50 and p95, per department | `metrics.sh` 02 |
| Fewer resources, or more work for the same | Pod-hours times requests, against what fixed runtimes reserved around the clock; peak tasks running per department | Kubernetes or the manager's metrics (issue 08 of two-services-with-ha); `metrics.sh` 09 |
| A new pod per task costs little | Run time per agent CLI, and per agent in both arms; hold hit rate | `metrics.sh` 05, 06; the audit log |
| Departments get their share | Wait per department while the budget is full, against guarantees and weights | `metrics.sh` 02; the dispatch page |
| Handoffs still work | Share of handed-off issues whose next agent failed or could not find the previous agent's work | Sampled by hand |

Comparison: move departments in batches (docs/DEPLOYMENT.md, "Moving departments"). Compare moved and not-yet-moved departments over the same weeks, each against its own weeks before the move, so a change in workload does not pass for a change of runtime.

## The baseline script

`metrics.sh SINCE UNTIL [OUT]` reads Multica's database with a read-only role and writes one CSV per question (01 to 09; `metrics.sql` says what each answers). Settings: `MULTICA_DATABASE_URL`; `MANAGER_DAEMON_ID` (the manager's `SANDBOX_MANAGER_DAEMON_ID`; its runtimes form the managed arm); `FIXED_DAEMON_IDS` (the departments' fixed runtimes; otherwise members' own machines count as fixed too). Run it once without `MANAGER_DAEMON_ID`, look at `01-runtimes.csv`, then set `FIXED_DAEMON_IDS`.

Written against the schema of the fork security/v0.11.0, and run against the kind dev database (both arms, 2026-11-26). ADR 0001's rule against reading Multica's database binds the manager, not an offline analysis with a read-only role. Recheck the columns after a Multica upgrade.

First step in the company, before any department moves: a baseline over the last four weeks.

## Stress tests, cheapest first

1. Simulated pods (`deploy/devpods/run.sh`, no Kubernetes): dispatch, fair share and the Postgres writes. 16 or more workspaces, bursts. Check the queue drains and shares follow guarantees and weights. Known limit: about one claim a second (docs/DEPLOYMENT.md, "Known limits"); compare with `08-arrivals.csv`.
2. Real pods, fake agent image, a test namespace: pod start time including image pull and the Istio sidecar, Kubernetes API throttling, quota refusals, several namespaces. Burst N tasks, N from half the budget to twice it; measure p95 wait. Inject faults and check no task fails: kill the manager leader (after step 2 of two-services-with-ha), a sandbox service replica (after step 3), delete pods by hand, fail over Postgres, restart Multica.
3. Real agents, one pilot department, one to two weeks, compared as above. Also covers what kind cannot: opencode over ACP, and the backend built from the fork.

## Tuning

| Setting | How to set it |
| --- | --- |
| `SANDBOX_POD_START_TIMEOUT` (3m) | p99 of measured pod start, plus margin |
| `SANDBOX_WARM_PODS` (2) | Peak arrivals per second times pod start seconds (Little's law). 6 a minute and 30 s to start is about 3 |
| `SANDBOX_HOLD_TIMEOUT` (10m) | p80 of the gap between consecutive tasks of one issue (`07-issue-gaps.csv`) |
| `SANDBOX_POD_MAX_IDLE` (24h) | Shorter if idle pods are rarely reused |
| `SANDBOX_MAX_PODS` (10) | The namespaces' quota minus warm pods; then by the share of time the budget is full and the p95 wait |
| Guarantee and weight per department (overrides file) | Use and priority; then which department waits longest while the budget is full |
| Pod CPU and memory (pod template) | p95 of real use and the out-of-memory rate; a second template if out-of-memory failures cluster on a few repositories |

## Order

1. Baseline from Multica's database, before any move.
2. Bring the first metrics forward (two-services-with-ha issue 08): wait, pods by state, failure reasons, pod start time. Until then only the audit log and the dispatch page show what happens.
3. Stress tests 1 and 2.
4. Pilot department, then batches.
