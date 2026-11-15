#!/bin/bash
# Run the manager on a simulated pool against a real Multica backend, with
# load, to watch the dispatch page (http://127.0.0.1:8791/dispatch).
#
# It starts the backend and its Postgres (the smoke stack's, in a compose
# project of its own), creates the bot account in three workspaces with
# different fair-share settings, starts cmd/sandbox-devpool on the bot's PAT,
# creates one agent per workspace and then opens issues at different rates
# until interrupted. Nothing runs in a simulated pod: each task is reported
# completed (or now and then failed) after a random while.
#
# Usage: deploy/devpool/run.sh   (from the repository root; Ctrl-C stops it)
# Settings (environment), besides lib.sh's:
#   DEVPOOL_PODS       simulated pods (6)
#   DEVPOOL_CAPACITY   slots per pod: tasks of one key running at once (3)
#   DEVPOOL_ARGS       extra sandbox-devpool flags, e.g. "-task-max 3m"
#   DEVPOOL_OVERRIDES  the overrides file's content (see below)
#   KEEP=1             leave the backend running; tear it down later with
#                      "deploy/devpool/run.sh down"
set -euo pipefail

export SMOKE_PROJECT=${SMOKE_PROJECT:-devpool}
SMOKE_OVERLAYS=""
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=../pool/compose/smoke/lib.sh
. "$root/deploy/pool/compose/smoke/lib.sh"

pods=${DEVPOOL_PODS:-6}
# acme shares pods between all its tasks (they are all the bot's, so under the
# person rule they share one key), up to the pod capacity at once; beta is
# under the workspace rule, whose pods every task of beta shares and which
# stay bound to beta, never reset for idling; gamma keeps the default
# person-issue rule, under which a comment's task reuses the issue's pod in
# hold.
overrides=${DEVPOOL_OVERRIDES:-"acme: {weight: 2, placement: person}
beta: {guarantee: 1, placement: workspace}
gamma: {guarantee: 2}"}

start_backend
step "bot account in three workspaces"
bot_account acme beta gamma
printf '%s\n' "$overrides" >"$state/overrides/overrides.yaml"
echo "overrides:"
sed 's/^/  /' "$state/overrides/overrides.yaml"

step "sandbox-devpool with $pods simulated pods"
bin=$state/sandbox-devpool
(cd "$root" && go build -o "$bin" ./cmd/sandbox-devpool)
# shellcheck disable=SC2086
"$bin" -multica "$API" -token-file "$state/secrets/pool-token" -overrides "$state/overrides/overrides.yaml" \
	-pods "$pods" -capacity "${DEVPOOL_CAPACITY:-3}" ${DEVPOOL_ARGS:-} >"$state/devpool.log" 2>&1 &
devpool=$!
cleanup() {
	kill "$devpool" 2>/dev/null || true
	wait "$devpool" 2>/dev/null || true
	[ "${KEEP:-}" = 1 ] || down
}
trap cleanup EXIT
# A signal ends the script through exit, so the cleanup above runs.
trap 'exit 130' INT TERM
echo "log: $state/devpool.log"
echo "dispatch page: http://127.0.0.1:8791/dispatch"

step "one agent per workspace"
declare -A agent
for i in 0 1 2; do
	ws=${workspaces[$i]}
	rt=$(claude_runtime)
	agent[$i]=$(make_agent "devpool-agent" "$rt")
	echo "workspace $ws: runtime $rt, agent ${agent[$i]}"
done

declare -A last
# open N IDX: open N issues in workspace IDX.
open() {
	local n=$1 i=$2 k prio
	ws=${workspaces[$i]}
	for k in $(seq "$n"); do
		prio=$(shuf -n1 -e none low medium high urgent)
		last[$i]=$(new_issue "devpool load $(date +%T) #$k" "${agent[$i]}" "$prio")
	done
}

# follow_up IDX: comment on workspace IDX's latest issue, which queues the
# issue's agent again: under person-issue its task reuses the issue's pod
# while that is in hold.
follow_up() {
	ws=${workspaces[$1]}
	[ -n "${last[$1]:-}" ] && comment "${last[$1]}" "devpool follow-up $(date +%T)"
}

step "load (Ctrl-C to stop)"
echo "acme: a burst of 8 every 3 minutes, sharing pods; beta: a new issue, then a follow-up on it, every 20 seconds each; gamma: 2 every 4 minutes"
tick=0
while :; do
	[ $((tick % 9)) -eq 0 ] && open 8 0 && echo "$(date +%T) acme +8"
	if [ $((tick % 2)) -eq 0 ]; then open 1 1; else follow_up 1; fi
	[ $((tick % 12)) -eq 6 ] && open 2 2 && echo "$(date +%T) gamma +2"
	tick=$((tick + 1))
	# Sleep in the background so a signal ends the script (and its cleanup)
	# at once, not after the sleep.
	sleep 20 &
	wait $!
done
