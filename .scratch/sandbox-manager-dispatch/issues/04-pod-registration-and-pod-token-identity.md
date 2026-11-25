# 04: Pod registration and pod token identity

**What to build:** Pods are identified by a pod token the manager issues, not by source IP, so that identity survives a mesh like Istio that rewrites addresses (ADR 0003).

- **Registration.** A new, unauthenticated registration endpoint on the manager assigns a pod identity and returns a pod token. The caller may pass a display label, such as the Kubernetes pod name.
- **Entrypoint.** The pod's entrypoint runs once per container start. It calls the endpoint, writes the token into the daemon's `.multica/config.json` in place of the placeholder, then starts the supervisor, which starts the daemon.
- **Presenting the token.** The daemon presents the token on every daemon-protocol call, and the supervisor presents it on the control channel.
- **Agent CLI traffic.** `mat_` calls are forwarded unchanged without identifying the pod.
- **Removed.** Source-IP identity, the self-reported-address checks and boot-nonce handling.

**Blocked by:** 03, 14

**Status:** resolved

- [x] Registering returns a pod token, and the manager resolves it to a pod with its label
- [x] A daemon or control call with an unknown or missing pod token is refused
- [x] Two pods behind the same source address are told apart by their tokens
- [x] A container restart registers a new pod; the old identity becomes lost after the heartbeat timeout and its leases end
- [x] Agent CLI calls carrying a `mat_` token are forwarded unchanged
- [x] The task-lifecycle harness and stock-daemon tests use token-registered pods instead of loopback addresses
- [x] Compose smoke: a pod registers through its entrypoint, runs a fake-agent task, resets and is lent again

## Comments

Verified on branch `worktree-agent-adfd9430dbb03ffc7` (2026-11-14), after merging the integration tip with tickets 05, 07 and 08:

- Tests (from `server/`, uid 1000): `go test -count=1 ./internal/sandboxmanager/... ./cmd/sandbox-manager/ ./internal/podsupervisor/ ./cmd/pod-supervisor/` 294 passed; `go test -race -count=1 ./internal/sandboxmanager/...` 247 passed; `go vet` and `gofmt` clean on those packages.
  - New manager tests (`pool_register_test.go`): registration returns a pod ID and an `mpt_` token, the label is display only and two same-labelled pods are two pods; a bad body is 400; every daemon and control call (workspace list, renew, register, heartbeat, both claims, task report, wake socket, control GET/POST) with no token, a forged token or a `mat_` token is 401 and causes no upstream call; two pods from one address are told apart (B cannot report on A's task); a restarted container registers as a new pod, the old one turns lost after the heartbeat timeout, its lease ends ("pod lost") and its task is failed upstream. Agent CLI: a `mat_` call is relayed unchanged even from a caller holding no pod token; a pod token on an agent path is 403.
  - Harness: `h.newPod(name)` plays the entrypoint (`POST /pod/register`) and all simulated pods dial from 127.0.0.1 with their token. Deleted with the code they tested: boot-nonce replacement, the `pod_ip`/SNAT check (`hardening_test.go` and two stock-daemon/liveness tests), the announcement window of the workspace list, "unknown pod heartbeat gets 404". A pruned pod's token is now refused (401).
  - Supervisor: polls and reports carry `Bearer <pod token>`; a 401 counts as an unknown pod like a 404 (the container restarts after `UnknownTimeout`); `New` requires the token.
- Images: `Dockerfile.manager`, `Dockerfile.pool --target fake` and the default `Dockerfile.pool` (`--build-arg AGENT_NPM_REGISTRY=https://registry.npmjs.org/`) build.
- Compose smoke, committed as `deploy/sandbox/pool/compose/smoke/smoke.sh` (+ `smoke.yaml`) for ticket 13 to extend: project `t04smoke`, its own Postgres, backend `multica-backend:pooldev` (the script defaults to `ghcr.io/multica-ai/multica-backend:v0.6.4`, not pulled here), torn down with `down -v`. The pod registered through its entrypoint (inventory name = container hostname, `.multica/config.json` holds an `mpt_` token), a daemon call without a token got 401, a fake-agent task completed (its `multica` comment went through the manager with the task's `mat_` token), the pod was reset and idle again, and a second task ran on the same pod finding 0 earlier task files. By hand with `KEEP=1`: `docker restart` of the pod gave a new pod ID with the same label; the old ID was lost after the heartbeat timeout.

Decisions:
- `POST /pod/register` `{"label": ...}` answers `{"pod_id", "token"}`. IDs are `pod-<12 hex>`, tokens `mpt_<64 hex>`; the inventory lists pods in registration order. Admin and audit JSON now say `id` / `pod_id` instead of `ip` / `pod_ip`.
- A new pod is reset once before it is lent (the old first-seen reset, now at registration). The lending policy learns of the pod at its daemon's first workspace registration; if that comes before the first reset is reported, the policy's Reset keeps the pending command instead of issuing a second one (`applyLocked`).
- After a manager restart every token is unknown (in-memory registry): daemon and control calls get 401, and each pod comes back only when its supervisor gives up after 5 minutes and the container restarts. Pods idle meanwhile; tasks stay queued in Multica.
- The entrypoint uses `POD_REGISTER_URL` (image: `${MANAGER_URL}/pod/register`), `POD_LABEL` (default hostname) and `POD_REGISTER_ATTEMPTS` (60, every 2 s), writes the token with `jq` into `.multica/config.json`, and passes that file to the supervisor (`--daemon-config`).
- `mat_` traffic is relayed for any caller, before any pod lookup; the task token is the credential.

Known risks / not verified:
- The registration endpoint is open (ADR 0003): anything that reaches the pod port can register and receive tasks. In compose, anything attached to the pod network can.
- The agent can read the pod token (same uid as the daemon, ticket 14).
- Not verified: the k8s manifests on a cluster (comments only changed, not rendered); a v0.6.4 backend.
