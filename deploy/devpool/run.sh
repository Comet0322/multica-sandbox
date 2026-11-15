#!/bin/bash
# Run the manager on simulated pods against a real Multica backend, with load,
# to watch the dispatch page (http://127.0.0.1:8791/dispatch).
#
# It uses the Multica of the local kind namespace (deploy/k8s/dev/up.sh first),
# signs in a bot account of its own, makes sure it has three workspaces with
# different fair-share settings, starts cmd/sandbox-devpool on the bot's PAT,
# creates one agent per workspace and then opens issues at different rates
# until interrupted. Nothing runs in a simulated pod: each task is reported
# completed (or now and then failed) after a random while, and no Kubernetes
# pod is created.
#
# Usage: deploy/devpool/run.sh   (from the repository root; Ctrl-C stops it)
# Settings (environment):
#   DEVPOOL_PODS       simulated pods (6)
#   DEVPOOL_ARGS       extra sandbox-devpool flags, e.g. "-task-max 3m -hold 5m"
#                      (flags: -pods -task-min -task-max -fail-rate -start
#                      -hold -sync -dispatch -models -overrides -admin
#                      -multica -token-file)
#   DEVPOOL_OVERRIDES  the overrides file's content (see below)
#   DEVPOOL_WORKSPACES workspaces (3): beyond acme, beta and gamma come ws-04,
#                      ws-05, ... with default settings and random bursts
#   API                the Multica backend (http://127.0.0.1:18080)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
API=${API:-http://127.0.0.1:18080}
devstate=${DEV_STATE:-$root/deploy/k8s/dev/.state}
[ -f "$devstate/backend.env" ] || { echo "run deploy/k8s/dev/up.sh first" >&2; exit 1; }
code=$(sed -n 's/^MULTICA_DEV_VERIFICATION_CODE=//p' "$devstate/backend.env")
state=${DEVPOOL_STATE:-${TMPDIR:-/tmp}/devpool-state}
mkdir -p "$state"
chmod 700 "$state"

step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
# wait_for DESCRIPTION SECONDS COMMAND...: run COMMAND every 2s until it
# succeeds.
wait_for() {
	local what=$1 secs=$2
	shift 2
	for _ in $(seq $((secs / 2))); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 2
	done
	fail "timed out waiting for $what"
}
# call METHOD PATH [BODY]: the backend's API as the bot, in workspace $ws.
call() {
	curl -fsS -X "$1" "$API$2" -H "Authorization: Bearer $jwt" -H "X-Workspace-ID: $ws" \
		-H 'Content-Type: application/json' ${3:+-d "$3"}
}
# claude_runtime waits for the manager's claude runtime in $ws and prints its
# ID.
claude_runtime() {
	rt_present() { call GET /api/runtimes | jq -e '[.[] | select(.provider == "claude")] | length > 0' >/dev/null; }
	wait_for "the manager's runtime in $ws" 60 rt_present
	call GET /api/runtimes | jq -r '[.[] | select(.provider == "claude")][0].id'
}
# make_agent NAME RUNTIME: create an agent in $ws.
make_agent() {
	call POST /api/agents "{\"name\":\"$1\",\"runtime_id\":\"$2\",\"visibility\":\"workspace\",\"max_concurrent_tasks\":10}" | jq -r .id
}
# new_issue TITLE AGENT [PRIORITY]: create an issue in $ws assigned to AGENT,
# which queues a task, and print its ID.
new_issue() {
	call POST /api/issues "{\"title\":\"$1\",\"assignee_type\":\"agent\",\"assignee_id\":\"$2\",\"priority\":\"${3:-none}\"}" | jq -r .id
}
# comment ISSUE TEXT: comment on ISSUE as the bot, which queues a new task of
# its agent.
comment() {
	call POST "/api/issues/$1/comments" "{\"content\":\"$2\"}" >/dev/null
}

pods=${DEVPOOL_PODS:-6}
# acme has twice the others' share of the pods beyond the guarantees; beta
# keeps one pod for itself, gamma two. beta's follow-ups land on their
# issue's held pod while the hold lasts.
overrides=${DEVPOOL_OVERRIDES:-"acme: {weight: 2}
beta: {guarantee: 1, hold_timeout: 10m}
gamma: {guarantee: 2}"}

slugs=(acme beta gamma)
for n in $(seq 4 "${DEVPOOL_WORKSPACES:-3}"); do
	slugs+=("$(printf 'ws-%02d' "$n")")
done

step "bot account in ${#slugs[@]} workspaces"
curl -fsS "$API/health" >/dev/null || fail "no Multica backend at $API"
curl -fsS -X POST "$API/auth/send-code" -H 'Content-Type: application/json' -d '{"email":"devpool-bot@dev.test"}' >/dev/null
jwt=$(curl -fsS -X POST "$API/auth/verify-code" -H 'Content-Type: application/json' \
	-d "{\"email\":\"devpool-bot@dev.test\",\"code\":\"$code\"}" | jq -r .token)
workspaces=()
for slug in "${slugs[@]}"; do
	id=$(curl -fsS "$API/api/workspaces" -H "Authorization: Bearer $jwt" | jq -r --arg s "devpool-$slug" '.[] | select(.slug == $s) | .id')
	if [ -z "$id" ]; then
		id=$(curl -fsS -X POST "$API/api/workspaces" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
			-d "{\"name\":\"$slug\",\"slug\":\"devpool-$slug\"}" | jq -r .id)
	fi
	workspaces+=("$id")
done
(umask 077 && curl -fsS -X POST "$API/api/tokens" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
	-d '{"name":"devpool"}' | jq -j .token >"$state/pool-token")
# The overrides are keyed by slug.
printf '%s\n' "$overrides" | sed -E 's/^([a-z0-9-]+):/devpool-\1:/' >"$state/overrides.yaml"
echo "overrides:"
sed 's/^/  /' "$state/overrides.yaml"

step "sandbox-devpool with $pods simulated pods"
bin=$state/sandbox-devpool
(cd "$root" && go build -o "$bin" ./cmd/sandbox-devpool)
# shellcheck disable=SC2086
"$bin" -multica "$API" -token-file "$state/pool-token" -overrides "$state/overrides.yaml" \
	-pods "$pods" ${DEVPOOL_ARGS:-} >"$state/devpool.log" 2>&1 &
devpool=$!
cleanup() {
	kill "$devpool" 2>/dev/null || true
	wait "$devpool" 2>/dev/null || true
}
trap cleanup EXIT
# A signal ends the script through exit, so the cleanup above runs.
trap 'exit 130' INT TERM
echo "log: $state/devpool.log"
echo "dispatch page: http://127.0.0.1:8791/dispatch"

step "one agent per workspace"
declare -A agent
for i in "${!slugs[@]}"; do
	ws=${workspaces[$i]}
	rt=$(claude_runtime)
	agent[$i]=$(make_agent "devpool-agent-$(date +%s)" "$rt")
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
# issue's agent again: its task lands on the issue's held pod while the hold
# lasts.
follow_up() {
	ws=${workspaces[$1]}
	[ -n "${last[$1]:-}" ] && comment "${last[$1]}" "devpool follow-up $(date +%T)"
}

step "load (Ctrl-C to stop)"
echo "acme: a burst of 8 every 3 minutes; beta: a new issue, then a follow-up on it, every 20 seconds each; gamma: 2 every 4 minutes"
[ "${#slugs[@]}" -gt 3 ] && echo "ws-04 and on: each tick, one in eight opens 1 to 4 issues"
tick=0
while :; do
	[ $((tick % 9)) -eq 0 ] && open 8 0 && echo "$(date +%T) acme +8"
	if [ $((tick % 2)) -eq 0 ]; then open 1 1; else follow_up 1; fi
	[ $((tick % 12)) -eq 6 ] && open 2 2 && echo "$(date +%T) gamma +2"
	for i in $(seq 3 $((${#slugs[@]} - 1))); do
		if [ $((RANDOM % 8)) -eq 0 ]; then
			k=$((RANDOM % 4 + 1))
			open "$k" "$i" && echo "$(date +%T) ${slugs[$i]} +$k"
		fi
	done
	tick=$((tick + 1))
	# Sleep in the background so a signal ends the script (and its cleanup)
	# at once, not after the sleep.
	sleep 20 &
	wait $!
done
