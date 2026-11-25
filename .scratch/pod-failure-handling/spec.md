# Pod failure handling

Branch `pod-failure-handling`, from `main` at 733d37f. Items 2 and 3 of the
Coder/OpenHands comparison; item 1 (agent CLI dies first on OOM) is dropped:
Kubernetes 1.28+ on cgroup v2 sets `memory.oom.group=1` on every container,
so an OOM kills every process in it whatever their OOM scores.

## Decisions

- Messages are English.
- Repeated transient start failures mark the pool broken after 3 in a row
  (`SANDBOX_POD_START_FAILURES`, default 3).
- A pod keeps its normal hold after a task fails (no change).

## Acceptance

General:
- G1 no new Kubernetes permission (pods only, six verbs).
- G2 gofmt, go vet, go test -race pass; the 8 e2e scenarios pass 3 runs in a row.
- G3 every new behaviour has unit tests and, where it needs a cluster, an e2e check.
- G4 correct after a manager restart.
- G5 DEPLOYMENT.md, ARCHITECTURE.md, GLOSSARY.md updated.
- G6 security scan of the branch shows nothing new.

2. Failure classification:
- 2.1 every failure has a phase (create, start, run), a kind (oom, evicted,
  unresponsive, deleted, exited, start-timeout, unschedulable, image,
  refused, invalid-template, api) and whether it is permanent; table tests.
- 2.2 messages say what happened and what to do next; tests check them.
- 2.3 the dispatch page and inventory show recent failures and counts by kind.

3. Stop when pods cannot start:
- 3.1 a permanent failure (image, refused, invalid-template) marks the pool
  broken at once; e2e with a missing image.
- 3.2 transient start failures mark it broken after N in a row; unit tests.
- 3.3 while broken, at most one probe pod starts at a time, within the
  backoff (up to 5m); tasks stay queued in Multica, none fails; ready pods
  that already exist still serve.
- 3.4 the first pod that gets ready clears it, with no restart; e2e loads the
  missing image into kind.
- 3.5 the dispatch page, the inventory and /readyz show broken and why.
