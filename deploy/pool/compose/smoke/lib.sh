# Shared by smoke.sh and scenarios.sh: a Multica backend with its own
# Postgres, the manager (stack.yaml) and the pods (pool.yaml), all in one
# compose project of their own, torn down with their volumes at the end.
#
# The caller sets SMOKE_PROJECT's default and SMOKE_OVERLAYS (extra compose
# files after smoke.yaml) before sourcing this file.
#
# Settings (environment):
#   MANAGER_IMAGE          manager image (multica-sandbox-manager:smoke)
#   POOL_IMAGE             fake pool image (multica-sandbox-pool:smoke-fake)
#   MULTICA_BACKEND_IMAGE  Multica backend (ghcr.io/multica-ai/multica-backend:v0.6.4)
#   SMOKE_PROJECT          compose project name; never "multica",
#                          "multica-selfhost" or "multica-sandbox"
#   SMOKE_API_PORT         host port of the backend on 127.0.0.1 (18204)
#   SMOKE_STATE            directory for secrets and overrides
#                          ($TMPDIR/$SMOKE_PROJECT-state)
#   KEEP=1                 leave the stack running; tear it down later with
#                          "<script> down"

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
compose_dir=$(dirname "$here")
export MANAGER_IMAGE=${MANAGER_IMAGE:-multica-sandbox-manager:smoke}
export POOL_IMAGE=${POOL_IMAGE:-multica-sandbox-pool:smoke-fake}
export MULTICA_BACKEND_IMAGE=${MULTICA_BACKEND_IMAGE:-ghcr.io/multica-ai/multica-backend:v0.6.4}
export SMOKE_API_PORT=${SMOKE_API_PORT:-18204}
case "$SMOKE_PROJECT" in multica | multica-selfhost | multica-sandbox)
	echo "SMOKE_PROJECT must not be $SMOKE_PROJECT" >&2
	exit 2
	;;
esac
state=${SMOKE_STATE:-${TMPDIR:-/tmp}/$SMOKE_PROJECT-state}
mkdir -p "$state/secrets" "$state/overrides"
chmod 700 "$state"
for f in dbpw jwt code; do
	[ -s "$state/$f" ] || (umask 077 && openssl rand -hex 12 >"$state/$f")
done
SMOKE_DB_PASSWORD=$(cat "$state/dbpw")
SMOKE_JWT_SECRET=$(cat "$state/jwt")
SMOKE_DEV_CODE=$(head -c 6 "$state/code" | tr 'a-f' '1-6')
export SMOKE_DB_PASSWORD SMOKE_JWT_SECRET SMOKE_DEV_CODE
export MULTICA_SERVER_URL=http://multica-backend:8080 PROVIDERS=claude
export SECRETS_DIR=$state/secrets OVERRIDES_DIR=$state/overrides
[ -e "$state/secrets/pool-token" ] || : >"$state/secrets/pool-token"
[ -e "$state/overrides/overrides.yaml" ] || : >"$state/overrides/overrides.yaml"
API=http://127.0.0.1:$SMOKE_API_PORT

dc() {
	local files=(-f "$compose_dir/stack.yaml" -f "$compose_dir/pool.yaml" -f "$here/smoke.yaml")
	local o
	for o in ${SMOKE_OVERLAYS:-}; do files+=(-f "$here/$o"); done
	docker compose -p "$SMOKE_PROJECT" "${files[@]}" "$@"
}
step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	dc logs --no-log-prefix --tail 60 manager >&2 || true
	exit 1
}
down() { dc down -v --remove-orphans 2>&1 | tail -n 3; rm -rf "$state"; }

if [ "${1:-}" = down ]; then
	down
	exit 0
fi
[ "${KEEP:-}" = 1 ] || trap down EXIT

inventory() { dc exec -T manager wget -qO- http://127.0.0.1:8791/v1/inventory; }
leases() { dc exec -T manager wget -qO- http://127.0.0.1:8791/v1/leases; }
# audit prints the manager's audit records (JSON lines on its stdout).
audit() { dc logs --no-log-prefix manager 2>/dev/null | grep '^{' | jq -c 'select(.action? != null)' 2>/dev/null || true; }
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

# start_backend starts the backend and its Postgres and waits for it.
start_backend() {
	step "backend ($MULTICA_BACKEND_IMAGE) and its Postgres"
	dc up -d multica-pg backend 2>&1 | tail -n 2
	wait_for "the backend" 120 curl -fsS "$API/health"
}

# login EMAIL prints a JWT for EMAIL (the backend runs with a fixed dev code).
login() {
	curl -fsS -X POST "$API/auth/send-code" -H 'Content-Type: application/json' -d "{\"email\":\"$1\"}" >/dev/null
	curl -fsS -X POST "$API/auth/verify-code" -H 'Content-Type: application/json' \
		-d "{\"email\":\"$1\",\"code\":\"$SMOKE_DEV_CODE\"}" | jq -r .token
}

# bot_account SLUG...: log the bot account in (jwt), create one workspace per
# slug (their IDs in the array workspaces, in order), and write the bot's PAT
# as the manager's secret.
bot_account() {
	jwt=$(login pool-bot@smoke.test)
	workspaces=()
	local slug
	for slug in "$@"; do
		workspaces+=("$(curl -fsS -X POST "$API/api/workspaces" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
			-d "{\"name\":\"$slug\",\"slug\":\"$slug\"}" | jq -r .id)")
	done
	(umask 077 && curl -fsS -X POST "$API/api/tokens" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
		-d '{"name":"pool-manager"}' | jq -j .token >"$state/secrets/pool-token")
	# The manager runs as 65532 and reads the secret and the overrides
	# through bind mounts.
	chmod 644 "$state/secrets/pool-token" "$state/overrides/overrides.yaml"
	chmod 755 "$state" "$state/secrets" "$state/overrides"
}

# call METHOD PATH [BODY]: the backend's API as the bot, in workspace $ws.
call() {
	curl -fsS -X "$1" "$API$2" -H "Authorization: Bearer $jwt" -H "X-Workspace-ID: $ws" \
		-H 'Content-Type: application/json' ${3:+-d "$3"}
}

# start_pool N: start the manager and N pods, wait until each pod registered
# through its entrypoint and finished its first reset, and set pods to their
# IDs.
start_pool() {
	step "manager and $1 pods"
	export POOL_SIZE=$1
	dc up -d --scale pod="$1" 2>&1 | tail -n 3
	local n=$1
	pool_registered() { inventory | jq -e --argjson n "$n" '[.pods[] | select(.state != "lost")] | length == $n' >/dev/null; }
	wait_for "$n pods to register" 120 pool_registered
	pool_ready() { inventory | jq -e --argjson n "$n" '[.pods[] | select(.state == "idle")] | length == $n' >/dev/null; }
	wait_for "the pods' first reset" 120 pool_ready
	pods=$(inventory | jq -r '[.pods[].id] | join(" ")')
}

# pod_container POD_ID prints the compose service index of the pod's
# container (its label is the container's hostname).
pod_container() {
	local name i
	name=$(inventory | jq -r --arg id "$1" '.pods[] | select(.id == $id) | .name')
	for i in $(seq "${POOL_SIZE:-1}"); do
		[ "$(dc exec -T --index "$i" pod hostname | tr -d '\r')" = "$name" ] && echo "$i" && return 0
	done
	return 1
}

# claude_runtime waits for the manager's claude runtime in $ws and prints its
# ID.
claude_runtime() {
	rt_present() { call GET /api/runtimes | jq -e '[.[] | select(.provider == "claude")] | length > 0' >/dev/null; }
	wait_for "the manager's runtime in $ws" 60 rt_present
	call GET /api/runtimes | jq -r '[.[] | select(.provider == "claude")][0].id'
}

# make_agent NAME RUNTIME [SLEEP]: create a fake-claude agent in $ws; with
# SLEEP its tasks stay running that many seconds.
make_agent() {
	local env='{}'
	[ -n "${3:-}" ] && env="{\"SANDBOX_FAKE_SLEEP\":\"$3\"}"
	call POST /api/agents "{\"name\":\"$1\",\"runtime_id\":\"$2\",\"visibility\":\"workspace\",\"max_concurrent_tasks\":10,\"custom_env\":$env}" | jq -r .id
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

# tasks ISSUE prints the issue's tasks as JSON lines {id,status,error},
# oldest first.
tasks() {
	call GET "/api/issues/$1/task-runs" | jq -c 'sort_by(.created_at) | .[] | {id, status, error}'
}

# wait_tasks ISSUE N STATUS SECONDS: wait until the issue has N tasks, the
# last of them in STATUS; print the last task's ID.
wait_tasks() {
	local issue=$1 n=$2 want=$3 secs=$4 st=""
	for _ in $(seq $((secs / 2))); do
		st=$(tasks "$issue" | jq -rs --argjson n "$n" 'if length >= $n then .[$n-1].status else "" end')
		[ "$st" = "$want" ] && tasks "$issue" | jq -rs --argjson n "$n" '.[$n-1].id' && return 0
		case "$st" in completed | failed | cancelled) [ "$st" != "$want" ] && fail "task $n of issue $issue ended $st, want $want" ;; esac
		sleep 2
	done
	fail "task $n of issue $issue is '$st' after ${secs}s, want $want"
}

# agent_comments ISSUE prints the fake agent's comments on ISSUE.
agent_comments() {
	call GET "/api/issues/$1/comments" | jq -r '.[]? | .content' | grep '^sandbox-fake: agent ran' || true
}

# lease_of TASK prints the pod that leased TASK.
lease_of() { leases | jq -r --arg t "$1" '[.[] | select(.task_id == $t)][0].pod_id // empty'; }

# ns TIME prints an RFC 3339 time in nanoseconds since the epoch, so times
# compare as numbers.
ns() { date -d "$1" +%s%N; }

# lease_time TASK prints the audit time of TASK's lease, in nanoseconds.
lease_time() {
	local t
	t=$(audit | jq -rs --arg t "$1" '[.[] | select(.action == "lease" and .decision == "allow" and .task_id == $t)][0].time // empty')
	[ -n "$t" ] || fail "no lease of task $1 in the audit log"
	ns "$t"
}

# resets_of POD prints the reasons of the pod's resets that started, one per
# line.
resets_of() {
	audit | jq -r --arg p "$1" 'select(.action == "reset" and .pod_id == $p and (.reason | startswith("reset started: "))) | .reason | ltrimstr("reset started: ")'
}

# pod_state POD prints the pod's state: idle, leased, resetting or lost.
pod_state() { inventory | jq -r --arg p "$1" '.pods[] | select(.id == $p) | .state'; }

# binding_of POD prints the pod's binding as JSON, null when it has none.
binding_of() { inventory | jq -c --arg p "$1" '.pods[] | select(.id == $p) | .binding'; }
