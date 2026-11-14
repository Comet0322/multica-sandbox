# Sandbox (pod image and pod supervisor)

The sandbox is what runs a task: a pod of one non-root container. PID 1 is
`pod-supervisor`, which starts the stock `multica daemon` as uid 1001 and
resets the pod when the manager's sandbox service asks; the daemon starts every agent CLI
itself, as the same user. Nothing runs as root or with a capability. The
sandbox manager is described in [POOL.md](POOL.md).

| File | What |
|---|---|
| `Dockerfile.pool` | The pod image (daemon, agent CLIs, pod supervisor) |
| `pool/entrypoint.sh`, `pool/pip.conf` | Image contents: supervisor entry point, package mirror settings |
| `home/` | Runtime-private agent configuration, kept read-only in the image (see `home/README.md`) |
| `fake/` | A stand-in agent CLI for tests without a model |
| `pool/k8s/sandbox/` | The pods' Deployment and kustomization |
| `pool/compose/pool.yaml` | The pods on a Docker host (`--scale pod=N`) |

## The pod

One container, user `sandboxd` (uid 1001, group 2000), read-only root
filesystem, all capabilities dropped, `no-new-privileges`. The cluster needs no
Kubernetes API for the pod and the image needs no root at run time.

The only writable path is the volume `/home/pod`, the daemon's home
(`POD_DAEMON_HOME`). Every place a task writes to is under it:

| What | Where |
|---|---|
| `HOME` of the daemon and the agents, so their caches (`~/.cache`, `~/.npm`, `~/.claude`, ...) | `/home/pod` |
| The daemon's working directory | `/home/pod` |
| Task directories (`MULTICA_WORKSPACES_ROOT`) | `/home/pod/workspaces` |
| `TMPDIR` of the daemon, and the base of each task's own `TMPDIR` (`MULTICA_AGENT_TEMP_BASE`) | `/home/pod/tmp` |
| `/tmp`, for programs that ignore `TMPDIR` | a symlink in the image to `/home/pod/tmp` |

At container start the entrypoint copies the daemon's CLI config
(`.multica/config.json`, manager address and token) and the image's Claude
skills (`.claude/skills`, from `home/`) into `/home/pod`, then starts the
supervisor. The supervisor's start clears `/home/pod` like a reset
(a restarted container keeps its volume).

A **reset** deletes everything under `/home/pod` except the skip list
(`POD_DAEMON_HOME_SKIP`, default `.multica,.claude/skills`). An entry is a path
relative to `/home/pod` and may be nested: `.claude/skills` is kept while the
rest of `.claude` (sessions, history, credentials) goes. Entries that leave
`/home/pod` are refused at startup. The reset carries on past what it cannot
delete, and then makes the empty `tmp` again. It stops no process and restores
no template; the supervisor reports the paths it could not delete. A task's
files therefore do not reach the next task, of whoever. What a reset does not
reach:

- **Processes.** A reset stops nothing, so a process a task left running (a
  detached server, a `sleep`) runs on into the next task, and can keep a file
  open or write new ones.
- **The skip list.** Whatever a task writes into `.multica` or
  `.claude/skills` survives, like the daemon's config and the image's skills,
  until the container restarts (the entrypoint then copies the image's skills
  in again).

The agents run as the daemon's user, so they can read the daemon's home,
including `.multica/config.json` with the pod's token, and see, signal and
trace the daemon; with the token, anything in the pod can call the manager as
the pod. The supervisor is out of their reach: it clears its dumpable
flag (no `ptrace`, no `/proc/<pid>/{mem,environ,fd}`), ignores the signals that
would end it except `SIGINT`/`SIGTERM` (as PID 1 it never gets `SIGKILL` or
`SIGSTOP` from inside the pod), and keeps spare threads so a fork bomb cannot
abort it. The compose file sets a process limit; set one on the cluster too
(kubelet `podPidsLimit`).

## Claude Code in the pod

Checked against Claude Code 2.1.314 with a fake Anthropic-style endpoint, the
daemon's argument list (`--strict-mcp-config --mcp-config <file>`, stream-json)
and an empty, even read-only, HOME:

- `ANTHROPIC_AUTH_TOKEN` + `ANTHROPIC_BASE_URL`, set in the agent's custom
  environment, are honoured: requests go to `<base>/v1/messages?beta=true` with
  `Authorization: Bearer <key>` and `anthropic-version: 2023-06-01`. No login
  or onboarding is needed. The model is the agent's `--model`, else Claude
  Code's own default (`claude-opus-5-5` for this version): the endpoint must
  serve it, and the small fast model (`ANTHROPIC_DEFAULT_HAIKU_MODEL`) if the
  deployment maps it elsewhere.
- An HTTP MCP entry (`type: http`, `headers.Authorization`) connects, lists
  and calls its tools; the Bearer header is sent on every POST.
- The image sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and
  `DISABLE_AUTOUPDATER=1`; without them Claude Code opens a connection to
  `api.anthropic.com` at start (telemetry). It is not fatal when blocked.
- Claude Code needs a writable HOME only to keep session state; it still runs
  with a read-only one, but `--resume` then has nothing to resume.
- If the models are not served by Anthropic's own API (Bedrock, Vertex, other
  vendors), add `ENABLE_TOOL_SEARCH=false` and
  `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` to the agent's `custom_env`: by
  default Claude Code sends the `advanced-tool-use`,
  `context-management` and other beta headers and defers MCP tools behind a
  `ToolSearch` tool, none of which such backends are guaranteed to accept.

## Agent CLIs in the image

The image carries Claude Code and opencode (1.19.5, the last 1.x release). Both
are listed in `AGENT_PACKAGES` and `AGENT_COMMANDS` of `Dockerfile.pool`; the
daemon starts them itself, as the pod's user.

opencode reaches an OpenAI-compatible endpoint through the provider in
`deploy/home/.config/opencode/opencode.json`, which the image names in
`OPENCODE_CONFIG` (it stays read-only in the image, outside HOME), and which reads
`OPENAI_BASE_URL` and `OPENAI_API_KEY` from the agent's custom environment.
Only that provider is enabled, so the model picker of an opencode agent shows
exactly the models the file names (`llm/<model>`). To offer another model, add
it to the file.

Auto-update and the model catalog download are switched off
(`OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_DISABLE_MODELS_FETCH`): the pods'
network policy allows neither.
