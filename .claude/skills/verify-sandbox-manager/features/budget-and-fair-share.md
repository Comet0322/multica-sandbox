# Budget and fair sharing

The manager never has more pods than its budget (`SANDBOX_MAX_PODS`). When the budget is full and a task waits, it deletes an idle pod before a held one, and never a pod within another department's guarantee. Departments (Multica workspaces) share the pods by guarantee and weight, set per workspace slug in the overrides file and reloaded on `SIGHUP`.

## Sub-features

- `budget-eviction` with the budget full, a task of department b deletes a's idle pod, not a's or b's held pods.
- `guarantee` with b's guarantee raised to 2, a task of a deletes a's own held pod, never one of b's.
- `fair-share-load` under sustained load across workspaces, the queue drains and each workspace's pods follow its guarantee and weight (simulated pods).

## How to get to it (user POV)

- Members of several departments queue more tasks than the budget allows; the operator sets guarantees and weights in the overrides file and watches the dispatch page.

## Driving it with e2e.sh

Preconditions:

- `doctor.sh` is ready.

- **Eviction order and guarantees.** Run `../scripts/e2e.sh scenarios`. `run.log` shows `PASS: budget: with 3 pods full, b's task deleted a's idle pod … and kept the held …; with b's guarantee 2, a's task deleted a's own held … and kept b's … and …`. `state/audit.jsonl` holds each `delete` with the task that caused it.
- **Fair share under load.** Run `deploy/devpods/run.sh` (no Kubernetes pods; simulated pods report tasks completed after a random while) and open `http://127.0.0.1:8791/dispatch`. Workspaces `devpods-acme` (weight 2), `devpods-beta` (guarantee 1) and `devpods-gamma` (guarantee 2) show their pods against their targets. Stop it with Ctrl-C, or `kill` the PID you started it with; its cleanup stops `sandbox-devpods`.

## Gotchas

- `deploy/devpods/run.sh` keeps opening issues until stopped, and its workspaces and agents stay in Multica (it reuses them on the next run). Its state is `${TMPDIR:-/tmp}/devpods-state`.
- The dispatch rate is about one task a second; a burst takes that long to fill pods (docs/DEPLOYMENT.md, "Known limits").
