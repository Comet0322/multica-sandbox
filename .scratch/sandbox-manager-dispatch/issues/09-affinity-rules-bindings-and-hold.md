# 09: Affinity rules, bindings and hold

**What to build:** A person's next matching task reuses the pod their last one used.

- **Affinity rules.** Affinity keys come from a registry of named rules: `task`, `person-issue` (the default; it uses the chat session for tasks without an issue), `person-agent` and `person`. Keys always include the initiator, so tasks of different people never share a binding. A task with no known initiator gets a unique key and is no longer refused.
- **Hold.** A binding with no running task enters hold and counts down. A matching task during hold takes the pod and restarts the countdown when it ends. When the hold runs out, the pod is reset.
- **Placement.** A claimed task goes to the most recently used binding of its key that has room. When every binding of its key is full, it opens a new binding with that key on the reserved pod.
- **Between leases.** Nothing is cleared between leases of one binding.

**Blocked by:** 07

**Status:** resolved

- [x] Each built-in rule produces the documented key, and a new rule can be added by registering one function
- [x] Tasks of two people never share a binding under any rule
- [x] A task without an initiator runs in a binding of its own
- [x] A matching task in hold reuses the pod, and the countdown restarts after it ends
- [x] A hold that runs out resets the pod; a hold timeout of 0 resets the pod as soon as its task ends
- [x] With several bindings of one key, the most recently used one with room is chosen
- [x] When every binding of the key is full, a new binding opens on the reserved pod and the task does not wait
- [x] The inventory shows each pod's binding, affinity key and hold countdown
- [x] The old issue and chat placement code paths are removed

## Comments

**Verified** (from `server/`, as uid 1000, on integration tip `cdedb77b9`, which includes tickets 04 and 08):

- `gofmt -l` (clean) and `go vet` pass on `./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/`.
- `go test -count=1` on those packages: 333 passed. Three ticket-10 tests are still skipped. `go test -race -count=1 ./internal/sandboxmanager/...`: 284 passed. The timing-based hold tests also passed 10 runs in a row under `-race`.
- Module seam (`lending/affinity_test.go`): table-driven scenarios with a fake clock.
  - Each rule's key, and a rule added by registering one function.
  - Never across people, under every rule. No initiator gives a unique key.
  - Hold expiry; renewal, with the countdown restarting at the next end; hold 0; a workspace hold override; a lost pod in hold.
  - Most recently used choice, across holds and running bindings.
  - `Binding()` reports key and hold.
- HTTP seam: the skipped ticket-09 harness tests were revived or rewritten. They cover sharing by issue, chat and person; the reserved pod opening a new binding; a task no pod can take staying queued upstream; hold, renewal and expiry (including the `hold expired` reset audit); a container event on a held pod; per-workspace rules; the inventory binding; and a task without an initiator running.
- Ticket 08's checkbox "Bindings in hold do not count against quota" is now verified by a `TestGlobalQueue` scenario.

**Not verified:** the compose smoke was not run.

**What changed.**

- Keys are `<rule>:<unit>/by:<initiator>/in:<workspace>`.
  - Units: `person-issue` uses `issue:<id>`, or `chat:<id>` when there is no issue. `person-agent` uses the agent ID. `person` uses `all`.
  - Keys name the workspace too, so one person's tasks in two workspaces do not share a pod.
  - No unit or no initiator gives `task:<id>`. The `task` rule always gives that.
- The initiator is `attribution.originator.id`, lower-cased (`claimedTaskRef.person()`). Audit and lease records keep `initiator_email`. The executor's no-initiator refusal is gone.
- A binding whose key is unique (`task:` prefix) resets as soon as its task ends, whatever the hold timeout, because nothing could renew it.
- Config:
  - `SANDBOX_PLACEMENT` takes `task`, `person-issue` (the default), `person-agent` or `person`. `issue` and `chat` are refused as unknown rules.
  - `SANDBOX_HOLD_TIMEOUT` (`PoolConfig.HoldTimeout`) replaces `SANDBOX_POD_IDLE_TIMEOUT`. The command line defaults it to 15m, and `0` resets at once. A `PoolConfig` zero value means 0, so library and harness pools reset at once unless a test sets a hold.
- `lending.WorkspaceSettings.HoldTimeout *time.Duration` exists (nil means the pool default), but the executor does not feed it yet; ticket 11 does.
- A pod in hold keeps executor state `leased`. The inventory's `binding` object (`affinity_key`, `tasks`, `used_slots`, `capacity_slots`, `hold`, `hold_until`, `hold_remaining_seconds`) distinguishes it.
- Hold expiry is checked in `Policy.Tick()`, which the executor calls from `markLostLocked`. That runs on every claim, inventory read and dispatch tick.
- A reservation is still only an idle pod. A bound or held pod never claims for itself: its key's next task is claimed against another idle pod's reservation, joins the binding, and the bound pod is woken. With no idle pod, a held pod's follow-up task stays queued upstream until the hold runs out. Ticket 10 fixes this.

**Hooks for ticket 10.**

- Eviction: make a held pod reservable in `reservable` / `claimOrder` as the last choice, ordered by idle time. Record when the hold started; `lastUsed` is only the last join. In `ClaimReturned`, a task of the hold's key joins it as now. Another key evicts it: reset, then place, or place after `ResetDone`.
- Maximum lifetime: `binding.openedAt` is set. `bindingFor` should skip bindings past it. `TaskEnded` should reset instead of holding. `Tick` (beside `expireHolds`) is where drain grace failures go.
- Forced resets for manager restart or removed workspace: `RemoveWorkspace` leaves holds in place today. A held binding of a removed workspace should reset there or in `Tick`.
