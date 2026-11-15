# Runbook: moving a department onto the manager

A department (a Multica workspace) moves by rebinding its agents from the
runtime it uses today to one of the manager's runtimes. Nothing else changes
for its members: issues, agents, skills and custom environments stay as they
are. Move departments one at a time, and keep the old runtime until the
department confirms.

Before the first department, the manager runs in the namespace and serves the
department's workspace (see the [deployment guide](../docs/DEPLOYMENT.md)):
its bot account is a member of the workspace, and the workspace's runtime list
shows the manager's runtimes online.

## 1. Choose the runtimes and the department's settings

- **Runtime kinds** (`SANDBOX_RUNTIMES`). `pool` serves most work: a pod is
  held for an issue between its tasks, then may serve another issue of the
  same department. Add `clean` when some of the department's work must run on
  a pod no other issue has touched. Enabling a kind registers its runtimes in
  every served workspace; it takes a manager restart.
- **The department's overrides** in `SANDBOX_WORKSPACE_OVERRIDES_FILE`, keyed
  by the workspace's slug. They apply on the next sync, without a restart:

  ```yaml
  <workspace-slug>:
    hold_timeout: 10m   # how long a pod waits for the issue's next task
    guarantee: 1        # pods kept for the department even when idle
    weight: 1           # its share of the pods beyond the guarantees
    quota: 4            # the most pods its running tasks may take; 0 is no cap
  ```

  Check `override_problems` in `/v1/inventory` afterwards: it must be empty.
- **The budget** (`SANDBOX_MAX_PODS`) must leave room for the department's
  guarantee and its usual number of tasks running at once, on top of the
  departments already moved.

## 2. Rebind the department's agents

In the Multica UI, open each of the department's agents and set its runtime to
the manager's:

- the **pool runtime** (`<agent CLI> (<SANDBOX_MANAGER_NAME>)`) for ordinary
  work;
- the **clean runtime** (`<agent CLI> (<SANDBOX_MANAGER_CLEAN_NAME>)`) for work
  that needs a pod of its own.

Rebind the agents of one squad together: a handoff between two agents works on
one pod only when both run on the same kind of runtime.

The next task of a rebound agent queues for the manager. Watch the first ones
on the dispatch page (`/dispatch` on the admin listener): each should go
`starting` or `warm`, then `running`, then `held`.

## 3. Keep the old runtime until the department confirms

Leave the department's old runtime (its machine or pod and its daemon) running
and registered until the department confirms that its work runs as before.
Tasks already queued for the old runtime still run there. While both run, an
agent's runtime is the one it is bound to, so nothing runs twice.

What to watch for in that time:

- **Out-of-memory failures** (`the pod ran out of memory (limit ...)` on the
  task in Multica): raise the pod template's memory limit if real work hits it.
- **Hold hits**: an issue's next task landing on its held pod (the lease's
  `pod_id` in `/v1/leases` matches the issue's previous task). Few hits with
  handoffs that need the previous agent's files mean the hold timeout is too
  short.
- **Pod start times** and **creation problems**: the dispatch page's banner and
  each starting pod's `reason`. Slow starts with tasks waiting mean more warm
  pods (`SANDBOX_WARM_PODS`).
- **Pod memory**: the pods' usage in the cluster's metrics, against the
  template's requests and limits.

Once the department confirms, remove the old runtime: stop its daemon, then
delete the runtime in Multica's runtime list.

## 4. Going back

Rebind the department's agents to the old runtime in the Multica UI. Tasks
already running on the manager's pods finish there; new tasks go to the old
runtime. Nothing on the manager needs to change. A pod the department leaves
behind is deleted when its hold and idle time end, or for the budget.

## 5. Tell the squad about handoffs

Tell the department's squads, before the move:

- **A handoff on the same pod works as before** while the pod is held for the
  issue: the next agent finds the previous one's branch in the pod's repo
  cache, unpushed or not.
- **A handoff across pods needs a push.** When the hold has ended, or the pod
  was deleted for the budget, the next task lands on another pod and sees only
  what the previous agent pushed through the git MCP. An agent that hands work
  on should push it before it ends.
- **Pool runtime pods are shared within the department.** After its hold, a pool runtime pod
  may serve another issue of the same department, even another person's, and
  that task sees the files left there. Work that must not be seen that way
  uses an agent bound to the clean runtime.
- **A task over the memory limit fails** with the reason in words, and only
  that task; Multica may retry it.
