Status: resolved

# Sandbox manager: dispatch, binding and reset

## Problem Statement

Members of the company's Multica workspaces need a cloud runtime ("Managed Sandbox") without running a daemon on their own machine. A sandbox manager already exists, but it does not fit how the company will run it:

- It identifies pods by their source IP. The company's Istio mesh rewrites source addresses, so every pod would look like the same pod.
- It also runs a per-task LiteLLM key issuer and a git/MCP gateway. The company already provides its own git MCP and LLM gateway as sidecars that agents use directly.
- The rules for lending pods are scattered across several files. They cannot express the behaviour the operator wants: keep a pod for the same person's next task for a while, renew that time when the person comes back, cap how long any pod lives, and take idle pods back when capacity runs out.
- It picks the next workspace to serve without looking at task priority across workspaces. It also gives each workspace its own daemon identity.
- It serves only a manager half. The pod half lives on a separate branch, so the system cannot be run or tested end to end.

The server it must work against is Multica v0.6.4.

## Solution

A single manager, running under one Multica bot account, looks like one ordinary daemon to the server.

- **Upstream identity.** It has one daemon identity across every workspace the account belongs to, and in each workspace one runtime per agent CLI, shared by all pods. Pods are invisible to the server.
- **Pod identity.** Each pod announces itself to the manager when its container starts and receives a pod token, which identifies it for as long as the container runs.
- **Dispatch.** The manager reads the pending queues of all workspaces, orders them the way the server does, and skips any workspace that has used up its quota. Before claiming a task, it reserves capacity on a pod, so every claimed task has somewhere to run immediately.
- **Lending policy.** One decision module owns all of it: which affinity key a task belongs to, whether a binding is full, how long a pod stays in hold, the maximum lifetime, and the eviction order. The rest of the manager carries out its decisions.
- **Reset.** Inside the pod, a reset only deletes the files under the daemon home, except the configured skip directories. It carries on past any file it cannot delete.
- **Credentials.** The manager handles no git or LLM credentials. Until the company sidecars are attached, agents use their own custom environment in Multica for both.

## User Stories

1. As an operator, I want to run one manager under one bot account, so that a single deployment serves every workspace that account belongs to.
2. As an operator, I want a workspace to be served as soon as the bot account joins it, so that I do not have to reconfigure the manager.
3. As an operator, I want a workspace the bot account leaves to drain its running tasks and then stop being served, so that leaving is safe.
4. As an operator, I want to set the runtime name members see through an environment variable, so that each deployment can be named for its audience.
5. As an operator, I want the manager to use one daemon identity in every workspace, so that the server sees one machine, not one per workspace.
6. As an operator, I want to override the daemon identity through an environment variable, so that I can keep it stable across redeployments or keep two environments apart.
7. As an operator, I want every pod in a workspace to share that workspace's runtime for each agent CLI, so that members see a single Managed Sandbox runtime per CLI.
8. As a member, I want to see only the manager's runtime and never individual pods, so that the pool behind it stays an implementation detail.
9. As the server, I want every daemon call I receive to come from the manager's identity, so that my ownership checks on tasks and runtimes pass for tasks a pod runs.
10. As an operator, I want pods to be identified by a token the manager issued, so that pod identity survives a mesh that rewrites source addresses.
11. As a pod, I want to register with the manager before my daemon starts and receive a pod token, so that my daemon and supervisor can present it on every call.
12. As a pod, I want my pod token written into the daemon's configuration, so that the stock daemon presents it without any change to the daemon.
13. As a pod, I want my supervisor's control channel to present the same pod token, so that reset commands reach the right pod.
14. As an operator, I want the manager to assign each pod's identity itself and keep the Kubernetes pod name only as a label, so that a pod cannot claim another's identity by naming itself.
15. As an operator, I want a container that restarts to register as a new pod, so that no state is carried across a restart by accident.
16. As an operator, I want the identity of a pod that went silent to be marked lost after the heartbeat timeout and its running tasks failed, so that work is never stuck on a vanished pod.
17. As an agent, I want my `multica` CLI calls with my task token to reach the server through the manager unchanged, so that comments, status changes and squad wake-ups behave as on a local daemon.
18. As an operator, I want the manager to choose the next task by task priority across all workspaces, oldest first within the same priority, so that urgent work is not stuck behind a busy workspace.
19. As an operator, I want a workspace that has used its quota to be skipped and the next task in the queue taken instead, so that one workspace cannot block the others.
20. As an operator, I want quota to count only capacity used by running tasks, so that pods in hold do not count against a workspace.
21. As an operator, I want the manager to claim a task only when a pod can run it, so that the server never hands out a task that then times out in the manager.
22. As an operator, I want the manager to reserve capacity before it claims, so that a claimed task always starts without waiting for a pod.
23. As an operator, I want a reservation released when the claim returns no task, so that capacity is not leaked.
24. As an operator, I want tasks that no pod can take to stay queued in Multica, so that members see them waiting rather than failed.
25. As an operator, I want every pod to run the same image, so that any task can go to any pod.
26. As an operator, I want each pod's capacity expressed as an amount of resources, with each task needing an amount, so that dispatch asks whether a task fits, not whether a pod is busy.
27. As an operator, I want capacity measured in slots for now, each task taking one, so that I can let one binding run several tasks at once.
28. As an operator, I want the capacity model to accept CPU and memory later from a usage source, so that dispatch can respect real load without being redesigned.
29. As a member, I want my next task on the same issue to run in the pod my last task used, so that the agent finds the working directory and session it left behind.
30. As a member, I want my tasks never to share a pod with another person's tasks, so that my files, environment and credentials are not visible to others.
31. As an operator, I want the affinity key to be a pluggable, named rule, so that adding a new sharing rule means adding one function.
32. As an operator, I want these affinity rules available: one task per binding, same person and same issue or chat (the default), same person and same agent, and same person, so that each workspace can trade isolation for reuse.
33. As an operator, I want a task with no known initiator to get a binding of its own, so that it never shares a pod with a person's work.
34. As an operator, I want a task whose affinity key matches several bindings to go to the most recently used one that has room, so that the freshest state is reused.
35. As an operator, I want a task whose affinity key matches only full bindings to start a new binding on the reserved pod, so that it never waits.
36. As an operator, I want nothing cleared between leases inside one binding, so that the next task continues exactly where the last one stopped.
37. As an operator, I want a binding with no running task to enter hold and count down, so that its pod is kept for the same affinity key for a while.
38. As an operator, I want a matching task that arrives during hold to take the pod and restart the countdown when it ends, so that an active conversation keeps its pod.
39. As an operator, I want a binding whose hold runs out to be reset, so that idle pods return to the pool.
40. As an operator, I want a hold timeout of zero to reset the pod as soon as its task ends, so that the "one task per pod" behaviour needs no separate rule.
41. As an operator, I want every binding to have a maximum lifetime, after which it takes no new tasks and is reset once its tasks end, so that no pod accumulates days of state.
42. As an operator, I want tasks that outlast a binding's maximum lifetime by more than a grace period to be failed, so that a stuck task cannot hold a pod forever.
43. As an operator, I want a binding in hold to be evicted when a task with a different affinity key needs a pod and none is free, so that hold never shrinks usable capacity.
44. As an operator, I want eviction to take the binding that has been idle longest first, so that the most recently active people keep their pods.
45. As a member, I want a pod running my task never to be evicted, so that my running work is never interrupted for someone else's.
46. As an operator, I want pods reset when they are lost, when the manager restarts, or when their workspace is removed, so that no binding outlives the state that justified it.
47. As an operator, I want pool-wide defaults for affinity rule, hold timeout, maximum lifetime and quota, with per-workspace overrides in the manager's configuration keyed by workspace slug, so that workspaces can differ where they need to.
48. As an operator, I want a reset to delete only the files under the daemon home, except configured skip directories, so that files written only at container start, such as the daemon's configuration with its pod token, survive.
49. As an operator, I want a reset to carry on past any file it fails to delete, so that one stubborn file never stops a pod from returning to service.
50. As an operator, I want the supervisor to treat no file or directory as special beyond the skip list, so that reset behaviour is predictable and configured, not hard-coded.
51. As an operator, I want a reset not to stop processes, so that reset stays a file operation.
52. As an operator, I want a pod whose reset left files behind to be lent again with the failure logged, so that partial failures do not shrink the pool.
53. As an operator, I want to accept that a manager restart fails the tasks running at that moment, so that the manager can stay a single replica with in-memory state.
54. As an operator, I want every lending decision recorded in the audit log with task, initiator and pod, so that a task can be traced to the person and pod behind it.
55. As an operator, I want the inventory to show each pod's binding, hold countdown and capacity use, so that I can see why the pool is full.
56. As an agent author, I want my agent's custom environment (LLM key, git token) passed through to the agent in the pod, so that agents work before the company sidecars are attached.
57. As a maintainer, I want the manager free of LiteLLM and git gateway code, so that the deliverable contains only what this design needs.
58. As a maintainer, I want the manager and pod halves developed on one branch, so that the whole system can be built and tested together.
59. As a maintainer, I want the manager to work against a Multica v0.6.4 server, so that it runs on the company's deployment.

## Implementation Decisions

### Upstream identity (manager ↔ server)

- The manager is the only party that talks to the server. Pods talk only to the manager. Agent CLI traffic carrying a task token (`mat_`) is forwarded unchanged, without identifying which pod sent it.
- The manager uses one daemon identity for every workspace. By default it is derived from the Multica URL and the bot account, and it can be overridden by an environment variable. It replaces today's per-workspace derivation from the URL and workspace ID.
- Runtimes cannot be unified across workspaces: the server stores a runtime per workspace and per agent CLI. Within a workspace, the manager registers one runtime per agent CLI. Every pod's registration is answered with that same runtime list, so pod calls refer to the manager's runtimes. This is how the code behaves today, and it must be preserved.
- The runtime name comes from an environment variable that already exists (default "Managed Sandbox"). Runtime visibility stays public by default.
- One bot account PAT. Workspace discovery and drain on leave stay as they are. There is no workspace allowlist.

### Pod identity

- The source-IP identity is removed, together with its self-reported-address checks and boot nonce handling.
- New manager endpoint: pod registration. It is unauthenticated for now. It assigns the pod an identity and returns a pod token. The caller may pass a display label, such as the Kubernetes pod name.
- The pod's entrypoint runs once per container start. It calls the registration endpoint, writes the pod token into the daemon's `.multica/config.json` in place of the old placeholder, and then starts the supervisor, which starts the daemon.
- The daemon presents the pod token on every daemon-protocol call, and the supervisor presents it on the control channel. The manager resolves the token to a pod, and an unknown token is refused.
- A token is valid until its container stops. A container restart registers a new pod. The old identity stops heart-beating, is marked lost after the heartbeat timeout, and its leases end.

### Lending policy module (new)

A pure, deterministic module with an injected clock and no network or HTTP. Its inputs are events and its outputs are decisions. Today's placement, lifetime, quota and claim-selection logic moves into it. The pool's HTTP and upstream code only executes the decisions.

- **Events in:** a pod registered; a pod became lost; a pod's reset completed; the pending-queue snapshot changed; a claim returned a task or returned none; a task ended; time passed; a workspace was added or removed; configuration changed.
- **Decisions out:**
  - claim from workspace W against reservation R, or release reservation R;
  - place task T in binding B on pod P, or open a new binding on reserved pod P;
  - reset pod P, with a reason;
  - fail task T, with a reason;
  - end the lease of task T.
- **Capacity.** A resource vector per pod and a demand vector per task. Only `slots` is implemented: pod capacity is configurable and every task demands 1. "Full" means the demand does not fit in what is free. The type leaves room for CPU and memory, which a later usage source will fill.
- **Quota** is per workspace, counted as the capacity occupied by that workspace's running tasks. Bindings in hold do not count.
- **Global queue.** Merge every served workspace's pending tasks, keeping only queued ones, ordered by priority descending and then creation time ascending. Walk the queue, skip workspaces without quota, and pick the first workspace whose task can be placed. Only one claim is in flight per reservation. The claim may return a different task of that workspace than the one seen, and that is accepted.
- **Reservation.** Before claiming, reserve a place that can certainly take a task:
  - a binding of a matching affinity key with free capacity, which is only known after the claim, so in practice reserve any free capacity;
  - an idle pod;
  - or a binding in hold that can be evicted.

  After the claim, compute the task's affinity key. If a binding with that key and free capacity exists, place the task there (most recently used first) and release the reservation. Otherwise open a new binding with that key on the reserved pod, evicting its hold if the reservation was one.
- **Affinity rules.** A registry of named functions from a task to an affinity key. Built-in rules:
  - `task`: a unique key per task;
  - `person-issue`: the default; the initiator plus the issue, or the chat session for tasks without an issue;
  - `person-agent`;
  - `person`.

  Keys always include the initiator. A task with no known initiator gets a unique key. Tasks without an initiator are no longer refused, because that refusal existed only for LiteLLM key ownership.
- **Binding lifecycle.**
  - A binding goes from running, to hold when no task is running, to reset.
  - A hold timeout of 0 resets immediately.
  - A matching task in hold renews the binding.
  - Maximum lifetime stops new placements, then resets the pod once its tasks end. Tasks still running after the drain grace period are failed.
  - Eviction applies only to holds, idle-longest first. A binding with running tasks is never evicted.
  - Forced resets happen when a pod is lost, when the manager restarts (every pod is reset and its running tasks failed upstream), and when a workspace is removed after the drain timeout.
- **Settings.** Pool defaults, with per-workspace overrides keyed by slug, for the affinity rule, hold timeout, maximum lifetime and quota. Pod capacity is pool-wide.

### Executor (existing pool code)

- The pool code keeps registration relay, heartbeats, task-call forwarding with ownership checks, reconciliation with the server, the maximum lease time, the audit log, the inventory and admin API, workspace sync and hot reload. These become executors of the module's decisions.
- Active dispatch over the daemon's wake socket remains the way a pod is told to claim at once. Polling stays as the fallback.
- The upstream claim is made against the chosen workspace's runtimes only after the module has granted a reservation.

### Pod reset (supervisor)

- A reset deletes everything under the daemon home except the configured skip directories. Errors are logged and skipped. No file or directory gets special treatment, nothing is restored from a template, and no process is stopped.
- The supervisor reports the reset as done, listing the paths it failed to delete. The manager logs them and lends the pod again.
- The supervisor's configuration is the daemon home and the skip-directory list.

### Removals

- The LiteLLM per-task key issuer, together with its configuration, environment, orphan-key handling and the budget, RPM and model settings.
- The git/MCP gateway program, its token registry, the verify endpoints and gateway listener, its configuration and environment injection, its deployment manifests and image, its NetworkPolicy rules, and the pool image's gateway credential helper.
- The source-IP identity, as described above.
- The old issue and chat placement code paths, now replaced by the module.

### Branch and compatibility

- The pod half (pod image, supervisor, runner, pod manifests) is merged into the development branch, and both halves are developed together.
- The base stays the v0.7.0 clean import. Every server call must exist and behave the same on v0.6.4: daemon register, heartbeat, claim, pending list, task start, progress and complete calls, workspace list, and runtime visibility update. Nothing may depend on migrations newer than v0.6.4.

## Testing Decisions

- A good test drives behaviour through a public seam and asserts outcomes visible at that seam: decisions emitted, calls the fake server received, files left on disk. It never asserts on internal maps, locks or helper calls.
- **Seam 1, the lending policy module.** This is the primary seam, and most behaviour is tested here as table-driven scenarios with a fake clock: event sequence in, decisions out. Scenarios cover:
  - affinity rules, including no initiator and never across people;
  - capacity and full bindings;
  - quota counting only running tasks;
  - global queue order and skipping workspaces without quota;
  - reservation granted, used and released;
  - hold renew and expiry, with a zero hold timeout;
  - maximum lifetime drain and failing tasks after the grace period;
  - eviction order, never evicting running bindings;
  - most-recently-used choice among several bindings;
  - forced resets;
  - per-workspace overrides.

  There is no prior art for a pure module here, so this is new.
- **Seam 2, the manager's HTTP boundary.** The existing task-lifecycle harness (fake Multica server, simulated pods and the real manager in one process) and the stock-daemon lifecycle tests are the prior art. They change from loopback-address pods to token-registered pods. They cover only wiring:
  - registration returns a token, and an unknown token is refused;
  - pending lists are read and claims sent to the chosen workspace;
  - a claim happens only with a reservation;
  - the fake server sees one daemon identity across workspaces and never sees pod details;
  - pods receive the shared runtime list;
  - task calls are forwarded with ownership checks;
  - `mat_` traffic passes through;
  - a restarted container registers as a new pod and the old one becomes lost.
- **Seam 3, the supervisor reset.** The existing supervisor tests are the prior art. They cover: deletes under the daemon home only, keeps the skip directories, continues past a file it cannot delete and reports it, and stops no process.
- **Not unit-tested.** The entrypoint script is covered by the existing compose end-to-end scenarios as a smoke test: a pod registers, runs a fake-agent task, resets and is lent again.
- **Carried over and removed tests.** The tests on the integration branch are brought over and adapted. Tests for LiteLLM, the git gateway and source-IP identity are deleted together with their code. Tests may stay on this lineage.

## Out of Scope

- Git and LLM credentials, which the company sidecars provide. Pods rely on the agent's custom environment until then.
- Authenticating the registration endpoint. It stays open for now (ADR 0003), and the registration endpoint is the only place to add authentication later.
- Manager high availability and keeping running tasks through a restart.
- Several pools against the same Multica server.
- Heterogeneous pools and matching task requirements to pod capabilities.
- Dispatch by CPU or memory. Only the capacity type leaves room for it.
- Resetting a pod when its issue is done or cancelled. The hold countdown covers this.
- A workspace allowlist, and weighted or guaranteed fair share between workspaces.
- Quarantining pods whose reset left files behind.
- Any use of the Kubernetes API by the manager.

## Further Notes

- Glossary: Pool, Pod, Pod token, Manager, Lease, Reset, Affinity key, Binding, Hold, Capacity, Quota, Reservation, Eviction (see `GLOSSARY.md`).
- ADRs: 0001 (pending queue, reserve then claim), 0002 (no git or LLM credentials in the manager), 0003 (pod identity by manager-issued token).
- **Known risk.** The registration endpoint is unauthenticated, so anything that can reach the manager can register as a pod. It would then receive tasks, including each agent's custom environment.
- Cleanup candidates to settle while implementing:
  - the legacy single-workspace configuration path, which the "no compatibility shims" rule suggests removing;
  - fields renamed from Token to Ticket whose comments still say Token.
- The pending-list read costs one request per served workspace per dispatch tick.
