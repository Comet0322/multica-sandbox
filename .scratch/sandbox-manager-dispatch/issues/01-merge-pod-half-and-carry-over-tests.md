# 01: Merge pod half and carry over tests

**What to build:** Put the manager and the pod half on one development branch, so the whole pool can be built and tested together. Merge the pod half: the pod image, supervisor, runner and pod manifests from the sandbox branch. Bring over the tests that live on the integration branch, so the current behaviour is pinned before anything changes. Behaviour does not change in this ticket.

**Blocked by:** None (can start immediately)

**Status:** resolved

- [x] The pod half is merged into the development branch without conflicts
- [x] The manager, supervisor and pod tests from the integration branch are present and pass with `go test` on the server module
- [x] The manager, git gateway and pod images build
- [x] The compose stack starts, a pod registers, and a fake-agent task runs to completion

## Comments

Verified on branch `worktree-agent-a9135e26f69347a78` (2026-11-14):

- Merge: `feat/sandbox-v0.7.0-clean` merged with `--no-ff`, no conflicts; `go build ./...` and `go vet` on the sandbox packages pass.
- Tests: `go test ./internal/sandboxmanager/ ./cmd/sandbox-manager/` 293 passed; `./internal/gitgateway/` 109 passed; `./internal/podsupervisor/ ./cmd/pod-supervisor/` 34 passed (run as a non-root user; the supervisor tests skip themselves as root).
  - Manager and gateway tests are the tip of `integration/sandbox-pool`; the non-test code there is identical to this branch, so no adaptation was needed.
  - The supervisor tests at the tip of `integration/sandbox-pool` (`supervisor_test.go`, `single_test.go`, `ipc_linux_test.go`) target the later single-container supervisor, which is not the pod half merged here. The merged supervisor/runner code is identical to `integration/sandbox-pool` at `a6a510cc9`, so the tests from that commit were taken instead (`supervisor_test.go`, `runner_test.go`, `agent_shim_test.go`, `agent_argfile_test.go`, `agent_gitdir_test.go`). The System V IPC test was dropped: the merged supervisor has no IPC clearing.
- Images: `Dockerfile.manager`, `Dockerfile.gateway`, `Dockerfile.pool --target fake` build. The default `Dockerfile.pool` target needs `--build-arg AGENT_NPM_REGISTRY=https://registry.npmjs.org/` off the company network (it otherwise installs from the internal package mirror); with it, it builds.
- Compose smoke (project `t01smoke`, own Postgres, torn down with `down -v`): `stack.yaml` + `gen-pods.sh 2` (fake pod image) + an override that turns LiteLLM off and adds a local Multica backend (`multica-backend:pooldev`, not v0.6.4) with a seeded bot account and workspace. Both pods registered, the manager registered a "Managed Sandbox" claude runtime, and an issue assigned to an agent on it ran: task queued then completed, and the fake agent's comment arrived through the manager.
- Not verified: LiteLLM key issuing and the git gateway against a real GitLab in the compose run; a v0.6.4 server (ticket 13).
