Status: resolved

# Step 1: the manager keeps its state in Postgres (one replica)

Goal of the whole effort: manager and sandbox service as two services, each highly available; the manager keeps the full dispatch record in Postgres so a restart or failover loses no task. Read `map.md` and `issues/01`..`07` in this folder for the agreed design. This step is the first of four (map.md, "Delivery order"); it must leave the program shippable on its own.

## Scope

In:

- A `Store` port in `internal/core` beside `TaskSource` and `Pool` (`internal/core/ports.go`). `internal/core` still imports only `internal/policy` (`imports_test.go`).
- A Postgres implementation in a new package `internal/store`, on `github.com/jackc/pgx/v5`, with migrations embedded and run with `github.com/pressly/goose/v3` at startup.
- Tables per `issues/02-dispatch-record-in-postgres.md`, minus what belongs to later steps: no `leader` table (step 2), no token hash column (step 3), no namespace column (step 4). Do not add columns for later steps.
- The core's record of held pods moves from pod annotations to the store: `Pool.Record` and the core's `Restore` from annotations go. The sandbox service's `Record`, `recordLoop`, `writeRecords`, `recordRefused` / `Halted` and the record annotations in `internal/sandbox/record.go` go with them, with their tests and docs. Pods then need no `patch` permission unless something else still patches; drop it from `deploy/k8s/rbac.yaml` and `deploy/k8s/dev/dev-user.yaml` if so.
- Startup: the core loads every pod not gone and every task not ended from the store. A pod the store holds and Kubernetes still has is taken over (the sandbox service must adopt it as held, keeping its pod token from the pod's env as today). A managed pod the store does not hold is deleted at startup, warm pods included; they are refilled. A pod the store holds that Kubernetes no longer has is lost: its task is failed as today.
- A task placed or handed but not started when the manager stopped is handed again, not failed: `reasonRestartedBeforeStart` goes. `Core.Next` hands the same task again to the same pod until the pod's start report marks it running.
- Failure reports to Multica go through a pending flag in `tasks`, set before the first attempt; a loop resends until Multica accepts, then clears it.
- The task payload column is encrypted (AES-256-GCM, key read from a file named by a setting, from a Kubernetes Secret), and set to NULL once the task is running.
- Ended tasks and gone pods are deleted after 30 days.
- `recoverOrphansOnce` in `internal/multica/tasksource/source.go` and the RecoverOrphans call in `internal/multica/runtimes.go` registration go: the store always knows what the manager holds, and a task claimed but never recorded is handed again by Multica after 90 s or failed as a retryable timeout at 300 s (issue 01, "What Multica v0.6.4 tolerates"). Keep the pod-side `recover-orphans` swallow in `daemonapi.go`.
- Settings: the database URL (from a file or env, follow how the PAT is read today) and the payload key file. Both required; no in-memory fallback.
- `deploy/k8s/dev`: a database for the manager on the existing dev Postgres (`multica-pg`) or a second one, and its URL in the dev state; `deploy/k8s/dev/manager.sh` and the e2e scripts pass it.
- Docs: `docs/ARCHITECTURE.md`, `docs/DEPLOYMENT.md` (settings, "What a pod serves, and a manager restart", known limits, the cutover in map.md "Deliverables and acceptance"), `GLOSSARY.md` where a term changes. Follow AGENTS.md.

Out: leader election, a second replica, splitting the sandbox service, HTTP API between the services, manager-issued tokens, several namespaces, metrics.

## Writes and their order (from issue 02)

Synchronous, before acting:

- after Multica returns a claim: insert the task (placed) with its payload;
- before `Next` returns a task to a pod: mark it handed;
- before the first failure report: set its pending flag.

May be asynchronous, latest wins: hold, idle and other pod record changes, task end.

## Acceptance

- `gofmt -l .` empty, `go vet ./...` and `go test -race ./...` green as a non-root user. Core logic tested with a fake `Store` through the core's public seams (follow `internal/core/fakes_test.go`). The Postgres store tested against a real Postgres when `SANDBOX_TEST_DATABASE_URL` is set, skipped otherwise; run them once against a throwaway Postgres container and say so.
- The existing e2e scenarios pass in the kind cluster (`deploy/k8s/e2e/scenarios.sh`, context `kind-sandbox-dev`; the `kind` binary is not on PATH, the cluster already runs).
- New e2e scenarios: restart the manager with a task placed but not started, the task runs and is not failed; restart with a task running, it finishes and its result reaches Multica.
- Leave no pod or manager running after a manual run. Never touch `multica-postgres-1` or the compose project `multica-selfhost`.

## Delivery

Commits on branch `two-services-ha-step1` (already created from `pod-failure-handling` at 96b05ee), small and single-purpose, conventional-commit subjects as in `git log`. Do not push. Commit messages end with:

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>

Report: what changed per commit, the test and e2e results with their output, anything in this brief you could not do or decided differently and why.

## Answer

Done on `two-services-ha-step1`, commits `beb18b3` to `62e76ae` (the `Store` port, `internal/store`, restore from the database, the dev database, the restart scenarios, docs). `go test -race ./...` with the store tests against the kind manager database, and `deploy/k8s/e2e/scenarios.sh` with both restart scenarios, passed on 2026-11-26. Next: step 2, leader election (map.md, "Delivery order"; issue 01).
