# Pod failures

A pod that cannot be created, does not start, or is lost once ready is classified by phase and kind, and the operator is told what to do. A task that runs out of memory fails with a reason a member can read, and only that task. When pods cannot start at all the pods are broken: one pod at a time tries, tasks wait queued in Multica, `/readyz` turns 503, and pods start again on their own once the cause is gone. Every pod is owned by the owner Deployment, so deleting it deletes the pods.

## Sub-features

- `oom` a task over its pod's memory limit fails with `the pod ran out of memory (limit 1Gi)`; the next task completes.
- `cannot-start` a template whose image does not exist breaks the pods at once; the inventory shows `broken` and a permanent `image` failure; `/readyz` is 503; loading the image recovers without a restart.
- `owner` every pod carries an owner reference to `sandbox-e2e-owner`; deleting that Deployment deletes the pods.

## How to get to it (user POV)

- A member's agent uses more memory than the pod template allows; the task's error in Multica says so.
- An operator ships a bad pod template or image; the dispatch page and `/v1/inventory` say why no pod starts.

## Driving it with e2e.sh

Preconditions:

- `doctor.sh` is ready, including `owner Deployment sandbox-e2e-owner`.
- `kind` or the stand-in `../scripts/kind` lists the cluster `sandbox-dev` (`e2e.sh` puts the stand-in on `PATH`), or the recovery half of `cannot-start` is skipped.

- **Out of memory.** Run `../scripts/e2e.sh scenarios`. `run.log` shows `PASS: out of memory: the task failed with 'the pod ran out of memory (limit 1Gi)…'; the next task completed`.
- **Owner.** The same run shows `PASS: owner: all N pods are owned by deployment sandbox-e2e-owner (not blocking its deletion)` and the deletion result.
- **Cannot start.** The same run shows `PASS: cannot start: a missing image broke the pods at once, one pod at a time tried, the task stayed queued and /readyz said why; once the image was loaded, pods started again without a restart and the task completed`.

## Gotchas

- `recovery not checked` or `deletion not checked` in a `PASS:` line means that half was skipped (no kind cluster, or no admin context `kind-sandbox-dev`); report it as skipped.
- Kind only: real ResourceQuota refusals, image pull secrets and the Istio sidecar's start time are not exercised here.
