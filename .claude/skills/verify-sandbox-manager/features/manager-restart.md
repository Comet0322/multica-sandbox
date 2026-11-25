# Manager restart

The manager keeps its dispatch record (pods held, tasks from claim to end) in its Postgres database. A restarted manager takes over from it: a running task runs on and its completion reaches Multica; a task placed on a pod but not yet started is handed again, never failed or retried. A pod the record holds that Kubernetes no longer has is lost, and its task fails in Multica.

## Sub-features

- `restart-running` a graceful restart while a task runs; the new manager relays its completion.
- `restart-before-start` a `SIGKILL` right after a task is placed on a pod that takes it only at its next poll; the new manager restores it as placed or handed and the task completes.
- `store` the Postgres store itself: migrations, writes, the encrypted payload, pruning (Go tests).

## How to get to it (user POV)

- An operator restarts the manager (a rollout, a crash) while members' tasks run or wait; members see their tasks complete as if nothing happened.

## Driving it with e2e.sh

Preconditions:

- `doctor.sh` is ready.
- For `store`, a Postgres the tests may create schemas in (`go-checks.sh` uses the kind manager database).

- **Store tests.** Run `../scripts/go-checks.sh`. It ends `store tests: ran against postgres://…/sandbox_manager`; a skip fails it.
- **Restart while running, and before start.** Run `../scripts/e2e.sh scenarios`. `run.log` shows `PASS: restart: task … ran on through a manager restart; the new manager relayed its completion to Multica` and `PASS: restart before start: task …, placed (or handed) and not started when the manager was killed, was handed again by the new manager and completed`.
- **Side effects.** `state/manager.log` holds both managers' logs one after the other; the second one's takeover lines show the pods and tasks it loaded from the database.

## Gotchas

- Leader election and a second replica (step 2 of `.scratch/two-services-with-ha/`) do not exist yet; a failover scenario does not exist either.
- Each e2e run gets a database of its own (`sandbox_e2e_<run>`), dropped at teardown; `KEEP=1` keeps it, and the run's workspaces, for inspection.
- The payload key in `.state/payload-key` must stay the one the database was written with; the manager does not start otherwise (docs/DEPLOYMENT.md, "Known limits").
