# 02: Remove LiteLLM and git gateway

**What to build:** The manager no longer handles any git or LLM credentials (ADR 0002). Remove all of the following:

- the per-task LiteLLM key issuer, with its configuration, environment injection and orphan-key handling;
- the git/MCP gateway program and its token registry;
- the gateway verify endpoints and the gateway listener;
- the gateway's deployment manifests, image and NetworkPolicy rules;
- the pool image's gateway credential helper.

Agents get git and LLM access from their own custom environment in Multica, which the stock daemon already passes through.

**Blocked by:** 01

**Status:** resolved

- [x] No LiteLLM or git gateway code, configuration, environment variable, manifest, image or Dockerfile remains
- [x] Tests that only covered LiteLLM or the gateway are deleted; all remaining tests pass
- [x] The manager starts with no LiteLLM or gateway settings and refuses none of the remaining settings
- [x] An agent's custom environment variables reach the agent in the pod unchanged
- [x] The compose stack runs a fake-agent task to completion without a gateway service

## Comments

Verified on branch `worktree-agent-a1129ee719da838bd` (2026-11-14), based on `feat/sandbox-manager-dispatch` at `97e847904`:

- Removed: `litellm.go`, `pool_gitgateway.go` and their wiring in `pool.go`, `pool_claim.go`, `pool_placement.go`, `pool_reconcile.go`; `server/internal/gitgateway/`, `server/cmd/git-gateway/`; the `SANDBOX_LITELLM_*`, `SANDBOX_GIT_GATEWAY_*` and `SANDBOX_POOL_GATEWAY_LISTEN` settings and the gateway listener in `cmd/sandbox-manager`; `Dockerfile.gateway`(+`.dockerignore`), `pool/k8s/git-gateway.yaml`, the gateway Service, ports, NetworkPolicy rules and kustomize image; the compose `git-gateway` service, `control` network and LiteLLM/GitLab secrets; `pool/git-credential-gateway` and the image's `GIT_GATEWAY_URL`/`GITLAB_URL` ARGs and git `insteadOf` rewrite. The opencode provider id was renamed `litellm` to `llm` (it reads `OPENAI_BASE_URL`/`OPENAI_API_KEY` from the agent's custom env). Docs (POOL, README, SANDBOX, GITOPS, compose and k8s READMEs) updated.
- Sweep: `grep -riE 'litellm|git-gateway|gitgateway|GIT_GATEWAY|credential-gateway|GATEWAY_LISTEN'` outside `.scratch/` and `docs/adr/` finds only unrelated upstream Multica comments in `server/pkg/{taskfailure,agent,llm}`.
- Tests: deleted every LiteLLM/gateway-only test (manager, cmd, `gitgateway`, `git-gateway`); the LiteLLM subtest of `TestLeaseMaxDefaultsAndValidation` and the credential checks of `TestLostPodEndsItsLeases` were dropped, the rest of those tests kept. Generic helpers (`enqueueTask`, `claimTask`, `envOf`, `eventually`) moved into `harness_test.go`. New: `TestAgentCustomEnvReachesThePodUnchanged` (harness: the claimed task's `agent.custom_env` reaches the pod as Multica sent it) and `TestManagerStartsWithEveryRemainingSetting` (every remaining env var loads, `NewPool` accepts it and the first sync works). Both passed on the first run: with LiteLLM off the behaviour already held, so they pin it rather than drive a change.
- `go build ./...`, `go vet` on the four sandbox packages, and `go test -count=1 ./internal/sandboxmanager/... ./cmd/sandbox-manager/... ./internal/podsupervisor/... ./cmd/pod-supervisor/...` as uid 1000: 273 passed.
- Images: `Dockerfile.manager`, `Dockerfile.pool --target fake`, and the default `Dockerfile.pool` target (with `--build-arg AGENT_NPM_REGISTRY=https://registry.npmjs.org/`) build.
- Compose smoke (project `t02smoke`, own Postgres, torn down with `down -v`): `stack.yaml` unchanged (manager only, no LiteLLM override needed) + `gen-pods.sh 2` (fake image) + a local Multica backend (`multica-backend:pooldev`, not v0.6.4). Both pods registered and went idle; an agent with `custom_env {"SANDBOX_FAKE_NOTE": ...}` on the "Managed Sandbox" runtime ran an issue task from queued to completed, and the fake agent's comment carried the note value, so the custom env reached the agent. The fake agent now echoes `SANDBOX_FAKE_NOTE` for this.
- Not changed: tasks with no initiator are still refused (spec moves that to the affinity work, ticket 09). `go mod tidy -diff` shows two stale requirements (`redismock`, `godebug`) that predate this ticket and are unrelated to the gateway. Not verified: a v0.6.4 server (ticket 13); the Kubernetes manifests were not applied to a cluster (`kubectl kustomize` of `pool/k8s` and `pool/k8s/sandbox` renders without error).
