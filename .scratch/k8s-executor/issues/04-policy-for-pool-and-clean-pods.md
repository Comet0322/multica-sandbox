# 04: Policy for pool and clean pods

**What to build:** `internal/lending` becomes `internal/policy` and decides with the rules of the spec's "Policy rules" section. It knows no Kubernetes and no Multica.

- **Keeps:** fair sharing (guarantee, weight, quota in whole pods), pending-list ordering, claim backoff, and probing.
- **Places** a claimed task by its kind:
  - pool: its key's held pod, then an idle pod of its department whose hold expired, then a warm pod, then a new pod;
  - clean: its key's held pod, then a warm pod, then a new pod.
- **Decides** whether a pod may be created against the budget and the warm count. When the budget is full it decides which pod to delete, in the spec's order, never taking a pod within a department's guarantee for another department.
- **Ends** holds at the hold timeout (clean: delete; pool: idle) and idle pool pods at the maximum idle lifetime.
- **Removes** bindings, slots, affinity rules and the person in a key, eviction waits, placement wait, maximum lifetime and drain grace.
- The import rule moves with it: `internal/policy` imports nothing else of this module. AGENTS.md names `internal/policy` instead of `internal/lending`.

**Blocked by:** 01

**Status:** resolved

- [x] Tests cover every placement order, both kinds, tasks with no key, budget-full deletion order within and across departments, guarantees, hold expiry for each kind and idle lifetime
- [x] No pod ever serves another department, and no clean pod another key, in a randomized test over many events
- [x] `internal/policy/imports_test.go` enforces the import rule
- [x] `gofmt`, `go vet` and `go test -race ./...` pass

## Comments

Branch `ticket/04-05-policy`, commit `82573e4`.

**Decisions:**
- **Budget and warm pods.** The sandbox service owns the warm count and never exceeds `SANDBOX_MAX_PODS`: it alone sees starting pods, their start timeout and the creation backoff. The policy owns who gets a pod: it is given the same budget (`kube.MaxPods()`) and, with each claim request, the live and starting pod counts. When a claim needs a pod and the pool has none ready, it asks for one (Acquire); only when the budget is full and nothing is starting does it delete one, which the service then replaces with a warm pod. With the GitOps service (no budget of its own) the budget counts as full whenever the pool has no free pod and none is resetting.
- **Deletion order:** the kind order (idle pool, held pool, held clean) comes first, then the workspace furthest over its share, then the pod held or idle longest. A pod a claim in flight was made against is never deleted.
- **A ready pod before each claim:** a pool claim is made against an idle pod of its workspace, else a warm pod; a clean claim against a warm pod only. The claimed task then goes where the placement order says; the reserved pod is released and keeps its phase.
- **A task whose pod was lost before it started** waits for the next pod and is failed after an internal 3 minutes (`policy.DefaultPodWait`, no setting). This replaces the placement wait.
- **Quota** counts running tasks and claims in flight, not held or idle pods (as the glossary says).
- **Kinds in the core already:** `core.Config.Kinds` and `manager.Config.Runtimes` came with this ticket, so the GitOps tests could run on clean tasks (reset after every task, as before). Issue 05 adds the second runtime set.
- **Kept but unused until 07:** `SANDBOX_PLACEMENT`, `SANDBOX_POD_CAPACITY`, `SANDBOX_PLACEMENT_WAIT`, `SANDBOX_POD_MAX_LIFETIME`, `SANDBOX_POD_DRAIN_GRACE`, and `placement`/`max_lifetime` in the overrides file are still read and checked but feed nothing; `deploy/POOL.md` says so. `SANDBOX_POD_MAX_IDLE` (default 24h) is new.
- The admin API keeps its `binding` shape, filled from the policy's view (key, hold, one task at most), and pods gain `phase` and `kind`; workspaces gain `idle_pods`. The fuller rework is issue 07.
- Manager tests on sharing, slots, eviction and maximum lifetime were replaced by tests of the new rules (handoff on the held pod, one task per pod, budget deletion, a pod never serving another workspace).
