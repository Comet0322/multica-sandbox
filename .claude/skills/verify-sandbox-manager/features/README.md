# Sandbox manager verification map

The maintained list of what users and operators can observe of the manager, and how to prove each with the harness in `../scripts/`. Read the index, then the feature file of the behaviour you changed.

## Baseline preconditions

- `../scripts/doctor.sh` prints `doctor: ready`: Multica v0.11.0 at `127.0.0.1:18080` in the kind namespace `multica-sandbox`, the fake-agent pod image loaded, no managed pod and no listener on 8791 to 8797.
- Each check runs its own manager, bot account, workspaces and database. Drive only a manager this run started.

## Driving conventions

- The user path is Multica's API as a member uses it: create an agent on one of the manager's runtimes, assign it an issue or comment on the issue, read the issue's task runs and comments. The scripts in `deploy/k8s/e2e/` do exactly that; prefer them over hand-made calls.
- The operator path is the admin API (`/v1/inventory`, `/v1/leases`, `/v1/dispatch`), the dispatch page (`/dispatch`) and `/readyz` on the pod listener.
- `scenarios.sh` runs every scenario in one fixed order; there is no selector. `smoke.sh` runs only the pool and clean tasks and the privacy check.

## Proof and skip reporting

- Proof is the scenario's `PASS:` line (it names what it observed: pods, task states, error text), the exit code, and the evidence directory `e2e.sh` printed.
- A scenario that prints `not checked` or `skipped` in its `PASS:` line did not verify that part. Report it as skipped, with the reason the line gives.
- A failed check prints `FAIL:`, the last 40 lines of the manager log and the inventory. Report those; never loosen a scenario to make it pass.

## Feature entry contract

Each feature file starts with an H1 title and one paragraph on the observable behaviour, then four H2 sections in this order: `Sub-features`, `How to get to it (user POV)`, `Driving it with e2e.sh`, `Gotchas`.

## Features

- [Run a task on a pod](./run-a-task.md): pool and clean tasks complete on real pods, holds and handoffs, and no pod detail reaches Multica.
- [Manager restart](./manager-restart.md): the dispatch record in Postgres lets a restarted manager take over running and placed tasks.
- [Pod failures](./pod-failures.md): out of memory, a pod that cannot start, the broken state, and the owner Deployment.
- [Budget and fair sharing](./budget-and-fair-share.md): which pod the full budget deletes, guarantees, weights and overrides.
- [Admin API and dispatch page](./admin-api-and-dispatch-page.md): what an operator reads, on a manager started by hand or on simulated pods.
