Status: open

# Multica v0.11.0 compatibility

The company runs the hardened fork `security/v0.11.0` of Multica (operator, 2026-11-26; at 738d38669, 19 commits on top of the v0.11.0 tag, 641 files changed). Every comparison and run targets that branch, not the bare tag. The manager, `internal/multica/daemonwire` fixtures, the kind dev namespace and the e2e scenarios all target v0.6.4, and so does the failover analysis in issue 01.

Rules: any code change this needs goes on its own branch, `multica-v0.11-compat`, from `pod-failure-handling` (96b05ee), never on the two-services branches. The two-services steps rebase onto it once it lands.

## Phase A: read, no code change (can run beside step 1)

Compare v0.6.4 and v0.11.0 in `../multica` (`git show v0.6.4:<path>` / `git show v0.11.0:<path>`) for everything the manager touches:

- the daemon HTTP and WebSocket protocol the manager plays toward the pods and uses toward Multica: every route in `internal/multica` (daemonapi.go, conn.go, client.go, runtimes.go, wake.go, models.go, registration.go, task.go) and the types in `daemonwire`; request and response fields, status codes, auth headers;
- what the v0.11.0 stock daemon in the pod calls that v0.6.4 did not, and what the manager would refuse (`daemonapi.go` `refuse`);
- the server behaviours issue 01 relies on: offline threshold, reconnect grace, reclaim of dispatched tasks (90 s), dispatch timeout (300 s), `dispatched_at` fencing, recover-orphans, the daemon's retry of terminal reports, what makes the daemon kill its agent;
- the pod image build (`deploy/Dockerfile.pool`, `--build-context multica=../multica`) against v0.11.0.

Output: `multica-v0.11-compat-report.md` here, one row per difference: what changed (file:line at each tag), whether the manager breaks, the fix, and whether issue 01's conclusions still hold.

## Phase B: run (after step 1 finishes; both use the kind cluster and port 18080)

- Point `deploy/k8s/multica` and `up.sh` at v0.11.0 images, build the pod image from `../multica` at v0.11.0, run `smoke.sh` and `scenarios.sh`.
- Fix what breaks on `multica-v0.11-compat`, update `daemonwire` fixtures, AGENTS.md, docs/DEPLOYMENT.md "Multica server version".
- v0.6.4 support is dropped (operator, 2026-11-26): the manager targets v0.11.0 only. Replace the v0.6.4 fixtures, images and wording outright; no code paths or tests kept for v0.6.4.
