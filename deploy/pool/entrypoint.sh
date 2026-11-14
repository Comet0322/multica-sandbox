#!/bin/sh
# Runs once per container start. It registers the pod with the manager, writes
# the pod token it gets into the daemon's CLI config, and becomes
# pod-supervisor (PID 1) with the pod layout taken from the image's
# environment. Everything after "--" is the daemon command. The supervisor
# passes the environment on to the daemon, so the daemon sees the image's
# MULTICA_* settings.
set -eu

: "${POD_CONTROL_URL:?set by the image}"
: "${POD_REGISTER_URL:?set by the image}"
# The label only names the pod in the manager's inventory and logs; the
# manager assigns the identity. In Kubernetes the hostname is the pod name.
label="${POD_LABEL:-$(hostname)}"
attempts="${POD_REGISTER_ATTEMPTS:-60}"

# The manager may still be starting: try again every two seconds, then give up
# so the container restarts.
body=$(jq -cn --arg l "$label" '{"label": $l}')
n=0
while :; do
	if answer=$(curl -fsS --max-time 10 -X POST -H 'Content-Type: application/json' \
		--data "$body" "$POD_REGISTER_URL"); then
		break
	fi
	n=$((n + 1))
	if [ "$n" -ge "$attempts" ]; then
		echo "multica-pod-entrypoint: could not register the pod at $POD_REGISTER_URL" >&2
		exit 1
	fi
	sleep 2
done
pod_id=$(printf '%s' "$answer" | jq -r '.pod_id // empty')
POD_TOKEN=$(printf '%s' "$answer" | jq -r '.token // empty')
if [ -z "$pod_id" ] || [ -z "$POD_TOKEN" ]; then
	echo "multica-pod-entrypoint: the manager answered no pod identity" >&2
	exit 1
fi
echo "multica-pod-entrypoint: registered as pod $pod_id ($label)" >&2

# The daemon's files written at container start. A reset deletes the rest of
# the daemon home and keeps these through the supervisor's skip list
# (POD_DAEMON_HOME_SKIP, which keeps .multica by default). The pod token replaces the
# template's placeholder; the daemon presents it on every call, and the
# supervisor reads it from here for the control channel.
umask 077
cp -R "$POD_DAEMON_HOME_TEMPLATE"/. "$POD_DAEMON_HOME"/
config="$POD_DAEMON_HOME/.multica/config.json"
POD_TOKEN="$POD_TOKEN" jq '.token = env.POD_TOKEN' \
	"$POD_DAEMON_HOME_TEMPLATE/.multica/config.json" >"$config.new"
mv "$config.new" "$config"
unset POD_TOKEN answer

# The image's Claude skills, which Claude Code reads from HOME only. The
# default skip list (".multica,.claude/skills") keeps them across resets; the
# rest of .claude goes. A container restart keeps the volume, so the image's
# copy replaces whatever is there.
skills=/etc/multica-sandbox/home-template/.claude/skills
if [ -d "$skills" ]; then
	chmod -R u+w "$POD_DAEMON_HOME/.claude/skills" 2>/dev/null || true
	rm -rf "$POD_DAEMON_HOME/.claude/skills"
	mkdir -p "$POD_DAEMON_HOME/.claude"
	cp -R "$skills" "$POD_DAEMON_HOME/.claude/skills"
fi

exec /usr/local/bin/pod-supervisor \
	--control-url "$POD_CONTROL_URL" \
	--daemon-home "$POD_DAEMON_HOME" \
	--daemon-config "$config" \
	${POD_DAEMON_HOME_SKIP:+--daemon-home-skip "$POD_DAEMON_HOME_SKIP"} \
	${POD_POLL_INTERVAL:+--poll-interval "$POD_POLL_INTERVAL"} \
	-- /usr/local/bin/multica daemon start --foreground
