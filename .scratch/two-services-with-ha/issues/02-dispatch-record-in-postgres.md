Type: grilling
Status: open
Blocked by: 01

# Dispatch record in Postgres

## Proposal

The core gets a third port, `Store`, beside `TaskSource` and `Pool`; `internal/store` implements it on Postgres with pgx. It replaces `Pool.Record` and `Restore` (pod annotations), and `internal/core` still imports only the policy.

Tables:

- `leader` (01).
- `pods`: namespace, name, token hash, state (creating, starting, ready, gone), the policy's record (phase, tenant, kind, key, since, until), created, ready, gone and why.
- `tasks`: task id, tenant, kind, key, pod, state (placed, handed, running, ended), payload, claimed, started, ended, end reason, and `fail_pending` for a failure report Multica has not accepted yet.
- `schema_version`, migrations embedded and run by the leader at takeover (goose).

When each write happens, synchronous unless noted:

- Before creating a pod: insert `pods` (creating), then call the sandbox service (03).
- After Multica returns a claim: insert `tasks` (placed) with the payload. A crash before the insert leaves the task dispatched upstream and unknown here, which Multica hands again after 90 s (01).
- Before handing a pod its task (`Core.Next`): mark it handed. `Next` hands the same task again to the same pod until the daemon's start report marks it running, so a lost response costs nothing.
- Task end, hold, idle, pod gone: written as they happen; these may be asynchronous (latest record wins), as `Record` is today, since a lost one is rebuilt from the daemon's replayed report and the sandbox snapshot.
- `Fail` upstream: set `fail_pending` first; a loop resends until Multica accepts, then clears it. Today this retry lives only in memory.

Takeover loads every pod not gone and every task not ended into the core, then reconciles with the sandbox snapshot. A task placed or handed but not running is handed again, no longer failed (`reasonRestartedBeforeStart` goes).

Payload: it carries the agent's custom environment, which may hold secrets. Encrypt the column with a key from a Kubernetes Secret, and null it once the task is running (until then `Next` may need it again).

Retention: delete ended tasks and gone pods after 30 days. The audit log stays JSON Lines as now.
