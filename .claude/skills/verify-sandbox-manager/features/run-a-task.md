# Run a task on a pod

A member assigns an issue to an agent bound to one of the manager's runtimes in Multica; the task runs on a pod of the manager's and completes in Multica. A pool runtime task may reuse an idle pod of its department; a clean runtime task gets a pod that served no other key. While an issue's pod is held, the issue's next task lands on it. Multica never shows a pod's name, IP or hostname.

## Sub-features

- `pool-task` a task of a pool runtime completes on a pod.
- `clean-task` a task of a clean runtime gets a pod that served no one, keeps it within its hold, and never shares it with another issue.
- `handoff-on-one-pod` agent B, handed agent A's issue while A's pod is held, lands on that pod and finds A's unpushed branch.
- `handoff-across-pods` after the hold and the idle pod are gone, B lands on a new pod and finds nothing of A's.
- `privacy` no pod name or IP appears in Multica's runtimes, issues, comments, task runs or messages; agents see the hostname `sandbox`.

## How to get to it (user POV)

- In Multica, create an agent on the manager's `claude` runtime (pool: daemon ID without `-clean`; clean: with it) and assign it an issue.
- Comment on the issue, or reassign it to another agent, to queue the next task of the same key.

## Driving it with e2e.sh

Preconditions:

- `doctor.sh` is ready.

- **Pool and clean task, privacy.** Run `../scripts/e2e.sh smoke`. `run.log` shows `PASS: tasks: pool task ran on sandbox-…, clean task on sandbox-…; both completed` and `PASS: privacy: none of N pod names or M pod IPs appear in … bytes …; agents saw the hostname 'sandbox'`, then `== smoke passed` and exit 0.
- **Clean runtime and handoffs.** Run `../scripts/e2e.sh scenarios`. `run.log` shows `PASS: handoff across pods: …`, `PASS: clean: …`, `PASS: handoff on one pod: …` and, at the end, `PASS: privacy: …`.
- **Side effects.** `state/multica-dump` holds what Multica showed (the fake agent's comments name the hostname and the files it found); `state/audit.jsonl` holds each lease and pod deletion with its task.

## Gotchas

- The fake agent comments `sandbox-fake: agent ran in sandbox …`; a real CLI is not exercised. Real agents (opencode over ACP, real models) are covered only by the rollout's pilot (`.scratch/rollout-evaluation/plan.md`).
- A task's run can take a minute when the warm pod is used up and a new pod must start; the scripts wait 180 s per task.
- `smoke.sh` passing says nothing about holds, handoffs or the clean runtime's isolation; those are only in `scenarios.sh`.
