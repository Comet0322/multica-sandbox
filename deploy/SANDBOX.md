# Sandbox (the pod image)

The sandbox is what runs a task: a pod of one non-root container that the
manager creates through the Kubernetes API from the operator's pod template.
PID 1 is `tini`, which runs the stock `multica daemon` as uid 1001; the daemon
starts every agent CLI itself, as the same user. Nothing runs as root or with
a capability. A pod runs one task at a time, never serves another department,
and is deleted, never reused across departments or cleaned for reuse. The
manager and the pod template are described in the
[deployment guide](../docs/DEPLOYMENT.md).

| File | What |
|---|---|
| `Dockerfile.pool` | The pod image (daemon and agent CLIs) |
| `pool/entrypoint-kube.sh`, `pool/pip.conf` | Image contents: the entrypoint, package mirror settings |
| `home/` | Runtime-private agent configuration, kept read-only in the image (see `home/README.md`) |
| `fake/` | A stand-in agent CLI for tests without a model |
| `k8s/pod.yaml` | The example pod template |

## The pod

One container, user `sandboxd` (uid 1001, group 2000), read-only root
filesystem, all capabilities dropped, no privilege escalation, no service
account token. The pod itself needs no Kubernetes API and the image needs no
root at run time.

The only writable path is the volume `/home/pod`, the daemon's home
(`POD_DAEMON_HOME`). Every place a task writes to is under it:

| What | Where |
|---|---|
| `HOME` of the daemon and the agents, so their caches (`~/.cache`, `~/.npm`, `~/.claude`, ...) | `/home/pod` |
| The daemon's working directory | `/home/pod` |
| Task directories (`MULTICA_WORKSPACES_ROOT`) | `/home/pod/workspaces` |
| `TMPDIR` of the daemon, and the base of each task's own `TMPDIR` (`MULTICA_AGENT_TEMP_BASE`) | `/home/pod/tmp` |
| `/tmp`, for programs that ignore `TMPDIR` | a symlink in the image to `/home/pod/tmp` |

At container start the entrypoint (`tini -- multica-pod-entrypoint-kube`, the
image's default) writes the daemon's CLI config (`.multica/config.json`, with
the manager's address and the pod token the manager set in
`SANDBOX_POD_TOKEN`), copies the image's Claude skills (`.claude/skills`, from
`home/`) into `/home/pod`, sets a private umask and becomes the daemon. A
daemon that exits ends the pod: it is not restarted, its task fails and the
pod is deleted.

What a task leaves in `/home/pod` stays for as long as the pod lives. A pool
runtime pod may serve another key of the same department, even another
person's task, once its hold ended, and that task sees those files and any
process left running. A clean runtime pod serves one key only. Nothing a pod
holds outlives the pod.

The agents run as the daemon's user, so they can read the daemon's home,
including `.multica/config.json` with the pod's token, and see, signal and
trace the daemon; with the token, anything in the pod can call the manager as
the pod, which reaches only the task placed on it. Set a process limit on the
cluster (kubelet `podPidsLimit`).

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
(`OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_DISABLE_MODELS_FETCH`): a pod is
not expected to reach the public internet.
