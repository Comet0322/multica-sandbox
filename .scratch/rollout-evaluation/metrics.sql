-- Rollout metrics from Multica's own database, read only.
--
-- Compares two arms over one time window: tasks that ran on the manager's
-- runtimes ("managed") and tasks that ran on the departments' long-lived
-- runtimes ("fixed"); tasks on any other runtime are "other". Written against the schema of the
-- Multica fork security/v0.11.0 (tables workspace, agent_runtime,
-- agent_task_queue).
--
-- Run through metrics.sh, which sets:
--   :since, :until   the window, on agent_task_queue.created_at
--   :daemon          the manager's daemon ID (SANDBOX_MANAGER_DAEMON_ID);
--                    its clean runtimes use the same ID followed by -clean
--   :fixed           comma list of the daemon IDs of the departments' fixed
--                    runtimes; empty means every runtime not the manager's
--                    (members' own machines included, so name them)
--   :out             the directory the CSV files go to

SET statement_timeout = '5min';

-- A temporary view lives in the session's own schema; it is the only thing
-- this script creates, and it goes with the session. Everything after it is
-- read only.
CREATE TEMP VIEW t AS
SELECT q.id, q.agent_id, q.issue_id, q.chat_session_id, q.runtime_id,
       q.status, q.failure_reason, q.error, q.attempt,
       w.slug AS workspace, r.provider, r.daemon_id, r.name AS runtime_name,
       CASE WHEN r.daemon_id IN (:'daemon', :'daemon' || '-clean') THEN 'managed'
            WHEN :'fixed' = '' OR r.daemon_id = ANY (string_to_array(:'fixed', ',')) THEN 'fixed'
            ELSE 'other' END AS arm,
       -- A task may be scheduled for later (fire_at); its wait starts then.
       GREATEST(q.created_at, COALESCE(q.fire_at, q.created_at)) AS ready_at,
       q.dispatched_at, q.started_at, q.completed_at
FROM agent_task_queue q
JOIN agent_runtime r ON r.id = q.runtime_id
JOIN workspace w ON w.id = r.workspace_id
WHERE q.created_at >= :'since' AND q.created_at < :'until';

SET default_transaction_read_only = on;

-- 1. Which runtimes fall in which arm, to check the daemon ID before trusting
-- anything below.
\set f :out '/01-runtimes.csv'
\o :f
SELECT arm, workspace, provider, daemon_id, runtime_name, runtime_id, count(*) AS tasks
FROM t GROUP BY arm, workspace, provider, daemon_id, runtime_name, runtime_id ORDER BY arm, workspace, provider;

-- 2. Waiting: from ready to started (what a member feels), and from ready to
-- dispatched (the share spent before any runtime took it). Seconds.
\set f :out '/02-wait.csv'
\o :f
SELECT arm, workspace, count(*) AS started,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY extract(epoch FROM started_at - ready_at))::numeric, 1) AS start_p50_s,
       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - ready_at))::numeric, 1) AS start_p95_s,
       round(max(extract(epoch FROM started_at - ready_at))::numeric, 1) AS start_max_s,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY extract(epoch FROM dispatched_at - ready_at))::numeric, 1) AS dispatch_p50_s,
       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM dispatched_at - ready_at))::numeric, 1) AS dispatch_p95_s
FROM t WHERE started_at IS NOT NULL
GROUP BY ROLLUP (arm, workspace) ORDER BY arm NULLS LAST, workspace NULLS FIRST;

-- 3. Outcomes: every terminal status and failure reason, with the share of
-- the arm's finished tasks.
\set f :out '/03-outcomes.csv'
\o :f
SELECT arm, status, COALESCE(failure_reason, '') AS failure_reason, count(*) AS tasks,
       round(100.0 * count(*) / sum(count(*)) OVER (PARTITION BY arm), 2) AS pct_of_arm
FROM t WHERE status IN ('completed', 'failed', 'cancelled')
GROUP BY arm, status, failure_reason ORDER BY arm, tasks DESC;

-- 4. Collateral failures: failed tasks that ended within 60 s of another
-- failed task on the same runtime. On a fixed runtime that is what one task
-- taking the daemon down (out of memory) does to the others; on the managed
-- runtimes a pod runs one task, so it should be near zero. runtime_recovery
-- is Multica failing what a restarted daemon had in flight.
\set f :out '/04-collateral.csv'
\o :f
WITH f AS (SELECT * FROM t WHERE status = 'failed' AND completed_at IS NOT NULL)
SELECT a.arm,
       count(*) AS failed,
       count(*) FILTER (WHERE EXISTS (
         SELECT 1 FROM f b WHERE b.runtime_id = a.runtime_id AND b.id <> a.id
           AND abs(extract(epoch FROM b.completed_at - a.completed_at)) <= 60)) AS failed_with_a_neighbour,
       count(*) FILTER (WHERE a.failure_reason = 'runtime_recovery') AS runtime_recovery,
       count(*) FILTER (WHERE a.error ILIKE '%out of memory%' OR a.error ILIKE '%oom%') AS mentions_oom
FROM f a GROUP BY a.arm ORDER BY a.arm;

-- 5. Run time of completed tasks, per arm and agent CLI. A new pod per task
-- adds clone and install time; this is where it shows.
\set f :out '/05-duration.csv'
\o :f
SELECT arm, provider, count(*) AS completed,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY extract(epoch FROM completed_at - started_at))::numeric, 1) AS run_p50_s,
       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM completed_at - started_at))::numeric, 1) AS run_p95_s
FROM t WHERE status = 'completed' AND started_at IS NOT NULL
GROUP BY arm, provider ORDER BY provider, arm;

-- 6. The same agents in both arms: median run time per agent in each, so a
-- change in the mix of work does not pass for a change of runtime.
\set f :out '/06-duration-paired.csv'
\o :f
WITH m AS (
  SELECT agent_id, arm, count(*) AS n,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM completed_at - started_at)) AS p50
  FROM t WHERE status = 'completed' AND started_at IS NOT NULL GROUP BY agent_id, arm)
SELECT f.agent_id, f.n AS fixed_n, round(f.p50::numeric, 1) AS fixed_p50_s,
       m2.n AS managed_n, round(m2.p50::numeric, 1) AS managed_p50_s,
       round((m2.p50 / NULLIF(f.p50, 0))::numeric, 2) AS managed_over_fixed
FROM m f JOIN m m2 ON m2.agent_id = f.agent_id AND m2.arm = 'managed'
WHERE f.arm = 'fixed' AND f.n >= 5 AND m2.n >= 5 ORDER BY f.agent_id;

-- 7. Gaps between consecutive tasks of one issue (next start minus previous
-- end). SANDBOX_HOLD_TIMEOUT should cover most of them; the p80 is a start.
\set f :out '/07-issue-gaps.csv'
\o :f
WITH s AS (
  SELECT workspace, issue_id, started_at,
         lag(completed_at) OVER (PARTITION BY issue_id ORDER BY started_at) AS prev_end
  FROM t WHERE issue_id IS NOT NULL AND started_at IS NOT NULL)
SELECT COALESCE(workspace, 'all') AS workspace, count(*) AS gaps,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY extract(epoch FROM started_at - prev_end))::numeric / 60, 1) AS gap_p50_min,
       round(percentile_cont(0.8)  WITHIN GROUP (ORDER BY extract(epoch FROM started_at - prev_end))::numeric / 60, 1) AS gap_p80_min,
       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM started_at - prev_end))::numeric / 60, 1) AS gap_p95_min
FROM s WHERE prev_end IS NOT NULL AND started_at >= prev_end
GROUP BY ROLLUP (workspace) ORDER BY workspace NULLS FIRST;

-- 8. Arrivals per minute: the busiest minutes, against the manager's claim
-- rate (about one task a second) and for sizing SANDBOX_WARM_PODS.
\set f :out '/08-arrivals.csv'
\o :f
WITH per_min AS (SELECT date_trunc('minute', ready_at) AS minute, count(*) AS n FROM t GROUP BY 1)
SELECT count(*) AS minutes_with_tasks,
       round(avg(n), 2) AS mean_per_min,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY n) AS p95_per_min,
       percentile_cont(0.99) WITHIN GROUP (ORDER BY n) AS p99_per_min,
       max(n) AS max_per_min
FROM per_min;

-- 9. Peak tasks running at once, per arm and department: how many pods each
-- department really needed, against what a fixed runtime reserved.
\set f :out '/09-concurrency.csv'
\o :f
WITH ev AS (
  SELECT arm, workspace, started_at AS at, 1 AS d FROM t WHERE started_at IS NOT NULL
  UNION ALL
  SELECT arm, workspace, completed_at, -1 FROM t WHERE started_at IS NOT NULL AND completed_at IS NOT NULL),
run AS (
  SELECT arm, workspace, at,
         sum(d) OVER (PARTITION BY arm, workspace ORDER BY at, d ROWS UNBOUNDED PRECEDING) AS running
  FROM ev)
SELECT arm, workspace, max(running) AS peak_running,
       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY running)::numeric, 1) AS p95_running_at_changes
FROM run GROUP BY arm, workspace ORDER BY arm, workspace;

\o
