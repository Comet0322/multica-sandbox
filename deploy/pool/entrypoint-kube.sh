#!/bin/sh
# Entrypoint of a pod the manager creates through the Kubernetes API. The
# manager set the pod token in SANDBOX_POD_TOKEN when it created the pod: this
# script writes the daemon's CLI config and becomes the stock daemon, under
# tini as PID 1 (the image's ENTRYPOINT is
# "tini -- multica-pod-entrypoint-kube"). The pod is deleted after its task,
# or after its hold or idle time; a daemon that exits ends the pod.
set -eu

: "${SANDBOX_POD_TOKEN:?the manager sets it when it creates the pod}"
: "${POD_DAEMON_HOME:?set by the image}"
: "${POD_DAEMON_HOME_TEMPLATE:?set by the image}"

umask 077
mkdir -p "$POD_DAEMON_HOME/tmp"
cp -R "$POD_DAEMON_HOME_TEMPLATE"/. "$POD_DAEMON_HOME"/
config="$POD_DAEMON_HOME/.multica/config.json"
# SANDBOX_MANAGER_URL, when the template sets it, replaces the address baked
# into the image.
jq --arg url "${SANDBOX_MANAGER_URL:-}" '.token = env.SANDBOX_POD_TOKEN | if $url != "" then .server_url = $url else . end' \
	"$POD_DAEMON_HOME_TEMPLATE/.multica/config.json" >"$config.new"
mv "$config.new" "$config"
unset SANDBOX_POD_TOKEN

# The image's Claude skills, which Claude Code reads from HOME only.
skills=/etc/multica-sandbox/home-template/.claude/skills
if [ -d "$skills" ]; then
	mkdir -p "$POD_DAEMON_HOME/.claude"
	cp -R "$skills" "$POD_DAEMON_HOME/.claude/skills"
fi

export HOME="$POD_DAEMON_HOME" TMPDIR="$POD_DAEMON_HOME/tmp"
cd "$POD_DAEMON_HOME"
exec /usr/local/bin/multica daemon start --foreground
