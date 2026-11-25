# 09: Move departments one by one

**What to build:** A runbook, and the first department moved.

- **Runbook** (`deploy/`):
  1. Choose `SANDBOX_RUNTIMES` and the department's overrides.
  2. Rebind the department's agents in the Multica UI to the manager's runtime: pool, or clean for work that needs isolation.
  3. Keep the department's old runtime until the department confirms.
  4. To go back, rebind the agents to the old runtime.
  5. Tell the squad that a handoff across pods needs a push through the git MCP.
- **First department:** one department is moved, watched for a week, and its pod memory, out-of-memory failures, hold hits and pod start times are recorded.

**Blocked by:** 08

**Status:** ready-for-human (the runbook is done, `deploy/RUNBOOK.md`; the first move remains for a human)

- [ ] The runbook exists and was followed for the first department
- [ ] The week's numbers are recorded in the Comments, with any change to pod size, hold timeout or warm count

## Comments

Branch `ticket/08-09-docs`, commit `e51d363`: the runbook is `deploy/RUNBOOK.md` (choose runtimes and overrides, rebind agents, keep the old runtime until the department confirms and what to watch meanwhile, going back, what to tell squads). It is linked from `docs/DEPLOYMENT.md` and the README.

**Still for a human:** moving the first department, watching it for a week, and recording pod memory, out-of-memory failures, hold hits and pod start times here. The first box stays open until the runbook has been followed for that department.

Before the move: the company's credentials must allow `patch` on pods (see 06), the pod template's requests and limits are still estimates (no real task's peak measured), and the manager can run in-cluster with `deploy/k8s/manager.yaml` (verified in kind) or on the operator's host with a kubeconfig.
