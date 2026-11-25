# Shared by scenarios.sh and smoke.sh: a manager on this host against a
# namespace with a Multica v0.11.0 backend in it, creating real pods there with
# the given kubeconfig. See README.md for the settings.
#
# Each run signs in a bot account of its own (e2e-<run>@<domain>), so it serves
# only the workspaces it creates, and on exit stops its manager, deletes every
# pod the manager labelled, archives its agents and deletes its workspaces.

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../../.." && pwd)
devstate=${E2E_DEV_STATE:-$root/deploy/k8s/dev/.state}
API=${E2E_API:-http://127.0.0.1:18080}
export KUBECONFIG=${E2E_KUBECONFIG:-$devstate/dev-user.kubeconfig}
[ -r "$KUBECONFIG" ] || {
	echo "no kubeconfig at $KUBECONFIG (set E2E_KUBECONFIG, or run deploy/k8s/dev/up.sh)" >&2
	exit 2
}
NS=${E2E_NAMESPACE:-$(kubectl config view --minify -o jsonpath='{..namespace}')}
NS=${NS:-default}
code=${E2E_CODE:-}
if [ -z "$code" ] && [ -f "$devstate/backend.env" ]; then
	code=$(sed -n 's/^MULTICA_DEV_VERIFICATION_CODE=//p' "$devstate/backend.env")
fi
[ -n "$code" ] || {
	echo "set E2E_CODE to the backend's MULTICA_DEV_VERIFICATION_CODE" >&2
	exit 2
}
image=${E2E_POD_IMAGE:-multica-sandbox-pod:kube-fake}
pull=${E2E_PULL_POLICY:-Never}
pod_port=${E2E_POD_PORT:-8796}
admin_port=${E2E_ADMIN_PORT:-8797}
if [ -n "${E2E_MANAGER_URL:-}" ]; then
	manager_url=$E2E_MANAGER_URL
else
	# kind's pods reach the host at the kind network's gateway.
	gw=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' | tr ' ' '\n' | grep -m1 '\.')
	manager_url=http://$gw:$pod_port
fi
ADMIN=http://127.0.0.1:$admin_port
# The Deployment that owns the pods (SANDBOX_POD_OWNER), when the kubeconfig
# may read it; the kind namespace has sandbox-e2e-owner (deploy/k8s/dev).
owner=${E2E_OWNER_DEPLOYMENT-sandbox-e2e-owner}
if [ -n "$owner" ] && ! kubectl get deployment "$owner" >/dev/null 2>&1; then owner=""; fi
LABEL=app.kubernetes.io/managed-by=sandbox-manager
run=${E2E_RUN:-$(date +%H%M%S)$((RANDOM % 90 + 10))}
state=${E2E_STATE:-${TMPDIR:-/tmp}/sandbox-e2e-$run}
mkdir -p "$state"
chmod 700 "$state"
k() { kubectl -n "$NS" "$@"; }

step() { printf '\n== %s\n' "$*"; }
passed=()
pass() {
	passed+=("$1")
	echo "PASS: $1"
}
fail() {
	echo "FAIL: $*" >&2
	[ -f "$state/manager.log" ] && tail -n 40 "$state/manager.log" >&2
	curl -fsS "$ADMIN/v1/inventory" 2>/dev/null | jq -c '{pods: [.pods[] | {name, phase, kind, workspace_id, key, task_id}], workspaces: [.workspaces[] | {slug, queued, occupied_pods}]}' >&2
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

# managed_pods prints the names of the pods carrying the manager's label.
managed_pods() { k get pods -l "$LABEL" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'; }
no_managed_pods() { [ -z "$(managed_pods)" ]; }
pod_exists() { k get pod "$1" >/dev/null 2>&1; }
pod_gone() { ! pod_exists "$1"; }

# Templates: the dev template with this run's image and manager address, and
# one whose image does not exist.
template() {
	sed -e "s|@MANAGER_URL@|$manager_url|" -e "s|image: multica-sandbox-pod:kube-fake|image: $1|" \
		-e "s|imagePullPolicy: Never|imagePullPolicy: $pull|" "$root/deploy/k8s/dev/pod.yaml"
}

# --- manager -----------------------------------------------------------------

manager=""
# start_manager [NAME=VALUE...]: start the manager with the base settings and
# these, and wait until its admin API answers.
start_manager() {
	[ -x "$state/sandbox-manager" ] || (cd "$root" && go build -o "$state/sandbox-manager" ./cmd/sandbox-manager)
	(
		export MULTICA_SERVER_URL=$API \
			SANDBOX_POOL_TOKEN_FILE=$state/pool-token \
			SANDBOX_POD_TEMPLATE=$state/pod.yaml \
			SANDBOX_NAMESPACE=$NS \
			SANDBOX_MANAGER_NAME="E2E $run" \
			SANDBOX_RUNTIMES=pool,clean \
			SANDBOX_PROVIDERS=claude \
			SANDBOX_MAX_PODS=4 \
			SANDBOX_WARM_PODS=1 \
			SANDBOX_POD_START_TIMEOUT=60s \
			SANDBOX_POOL_SYNC_INTERVAL=5s \
			SANDBOX_POOL_RECONCILE_INTERVAL=5s \
			SANDBOX_WORKSPACE_OVERRIDES_FILE=$state/overrides.yaml \
			SANDBOX_MANAGER_LISTEN=0.0.0.0:$pod_port \
			SANDBOX_POOL_ADMIN_LISTEN=127.0.0.1:$admin_port \
			SANDBOX_AUDIT_FILE=$state/audit.jsonl
		[ -n "$owner" ] && export SANDBOX_POD_OWNER=deployment/$owner
		for kv in "$@"; do export "${kv?}"; done
		exec "$state/sandbox-manager"
	) >>"$state/manager.log" 2>&1 &
	manager=$!
	wait_for "the manager's admin API" 30 curl -fsS "$ADMIN/v1/inventory"
}
stop_manager() {
	[ -n "$manager" ] || return 0
	kill "$manager" 2>/dev/null || true
	wait "$manager" 2>/dev/null || true
	manager=""
}
# reload: make the manager read the overrides file again at once.
reload() { kill -HUP "$manager"; }
# overrides YAML: write the overrides file (keys are this run's slugs a and
# b) and reload it.
overrides() {
	printf '%s\n' "$1" | sed -E "s/^(a|b):/e2e-$run-\1:/" >"$state/overrides.yaml"
	[ -n "$manager" ] && reload
	sleep 2
}

inventory() { curl -fsS "$ADMIN/v1/inventory"; }
leases() { curl -fsS "$ADMIN/v1/leases"; }
# lease_pod TASK prints the name of the pod that ran TASK.
lease_pod() {
	local id
	id=$(leases | jq -r --arg t "$1" '[.[] | select(.task_id == $t)][0].pod_id // empty')
	[ -n "$id" ] || fail "no lease of task $1"
	grep -m1 "^$id " "$state/pod-ids" | cut -d' ' -f2 | grep . || fail "pod $id of task $1 was not seen"
}
# phase_of POD prints the pod's phase in the manager's inventory.
phase_of() { inventory | jq -r --arg n "$1" '.pods[] | select(.name == $n) | .phase'; }
pod_in_phase() { [ "$(phase_of "$1")" = "$2" ]; }

# Every pod name, IP and manager ID seen during the run, for the privacy check
# and lease_pod (a pod's ID stays in the leases after it is deleted).
watch_pods() {
	# Runs in the background, through manager restarts: no errexit.
	set +e
	while :; do
		k get pods -l "$LABEL" -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}' 2>/dev/null >>"$state/pods-seen"
		curl -fsS "$ADMIN/v1/inventory" 2>/dev/null | jq -r '.pods[] | "\(.id) \(.name)"' >>"$state/pod-ids" 2>/dev/null
		sleep 1
	done
}

# --- Multica -----------------------------------------------------------------

login() {
	curl -fsS -X POST "$API/auth/send-code" -H 'Content-Type: application/json' -d "{\"email\":\"$1\"}" >/dev/null
	curl -fsS -X POST "$API/auth/verify-code" -H 'Content-Type: application/json' \
		-d "{\"email\":\"$1\",\"code\":\"$code\"}" | jq -r .token
}
# call METHOD PATH [BODY]: the backend's API as the bot, in workspace $ws.
call() {
	curl -fsS -X "$1" "$API$2" -H "Authorization: Bearer $jwt" -H "X-Workspace-ID: $ws" \
		-H 'Content-Type: application/json' ${3:+-d "$3"}
}
# bot_account SLUG...: sign in this run's bot, create its workspaces (IDs in
# the array workspaces, in order) and write its PAT for the manager.
bot_account() {
	jwt=$(login "e2e-$run@${E2E_EMAIL_DOMAIN:-dev.test}")
	[ -n "$jwt" ] && [ "$jwt" != null ] || fail "could not sign in the bot"
	workspaces=()
	local slug
	for slug in "$@"; do
		workspaces+=("$(curl -fsS -X POST "$API/api/workspaces" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
			-d "{\"name\":\"e2e $run $slug\",\"slug\":\"e2e-$run-$slug\"}" | jq -r .id)")
	done
	(umask 077 && curl -fsS -X POST "$API/api/tokens" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
		-d '{"name":"sandbox-e2e"}' | jq -j .token >"$state/pool-token")
}
# runtime KIND: wait for the manager's claude runtime of KIND (pool or clean)
# in $ws and print its ID.
runtime() {
	local sel='.daemon_id | endswith("-clean") | not'
	[ "$1" = clean ] && sel='.daemon_id | endswith("-clean")'
	rt_present() { call GET /api/runtimes | jq -e "[.[] | select(.provider == \"claude\" and ($sel))] | length > 0" >/dev/null; }
	wait_for "the manager's $1 runtime in $ws" 60 rt_present
	call GET /api/runtimes | jq -r "[.[] | select(.provider == \"claude\" and ($sel))][0].id"
}
# make_agent NAME RUNTIME [ENV_JSON]: create a fake-claude agent in $ws.
make_agent() {
	local id env=${3:-}
	[ -n "$env" ] || env='{}'
	id=$(call POST /api/agents "{\"name\":\"$1-$run\",\"runtime_id\":\"$2\",\"visibility\":\"workspace\",\"max_concurrent_tasks\":10,\"custom_env\":$env}" | jq -r .id)
	# Recorded in a file: callers run this in a command substitution.
	echo "$ws $id" >>"$state/agents"
	echo "$id"
}
# new_issue TITLE AGENT: create an issue in $ws assigned to AGENT, which
# queues a task, and print its ID.
new_issue() {
	call POST /api/issues "{\"title\":\"$1\",\"assignee_type\":\"agent\",\"assignee_id\":\"$2\"}" | jq -r .id
}
# assign ISSUE AGENT: hand the issue to another agent, which queues its task.
assign() { call PUT "/api/issues/$1" "{\"assignee_type\":\"agent\",\"assignee_id\":\"$2\"}" >/dev/null; }
# comment ISSUE TEXT: comment as the bot, which queues the issue's agent again.
comment() { call POST "/api/issues/$1/comments" "{\"content\":\"$2\"}" >/dev/null; }
# tasks ISSUE: the issue's tasks as JSON lines {id,status,error}, oldest first.
tasks() { call GET "/api/issues/$1/task-runs" | jq -c 'sort_by(.created_at) | .[] | {id, status, error}'; }
# wait_tasks ISSUE N STATUS SECONDS: wait until the issue's Nth task is in
# STATUS, and print its ID.
wait_tasks() {
	local issue=$1 n=$2 want=$3 secs=$4 st=""
	for _ in $(seq $((secs / 2))); do
		st=$(tasks "$issue" | jq -rs --argjson n "$n" 'if length >= $n then .[$n-1].status else "" end')
		[ "$st" = "$want" ] && tasks "$issue" | jq -rs --argjson n "$n" '.[$n-1].id' && return 0
		case "$st" in completed | failed | cancelled) fail "task $n of issue $issue ended $st, want $want: $(tasks "$issue" | jq -rs --argjson n "$n" '.[$n-1].error')" ;; esac
		sleep 2
	done
	fail "task $n of issue $issue is '$st' after ${secs}s, want $want"
}
# task_error ISSUE N: the Nth task's error.
task_error() { tasks "$1" | jq -rs --argjson n "$2" '.[$n-1].error // ""'; }
# last_comment ISSUE: the fake agent's latest comment on ISSUE.
last_comment() { call GET "/api/issues/$1/comments" | jq -r '.[]? | .content' | grep '^sandbox-fake: agent ran' | tail -n 1; }

# --- teardown ----------------------------------------------------------------

teardown() {
	set +eu
	[ -n "${watcher:-}" ] && kill "$watcher" 2>/dev/null
	stop_manager
	step "teardown"
	k delete pods -l "$LABEL" --wait=true --timeout=120s 2>&1 | tail -n 1
	local w a n=0
	while read -r w a; do
		ws=$w call POST "/api/agents/$a/archive" >/dev/null 2>&1 && n=$((n + 1))
	done < <(cat "$state/agents" 2>/dev/null)
	echo "archived $n agents"
	if [ "${KEEP:-}" != 1 ]; then
		for w in "${workspaces[@]:-}"; do
			[ -n "$w" ] || continue
			curl -fsS -X DELETE "$API/api/workspaces/$w" -H "Authorization: Bearer $jwt" >/dev/null 2>&1
		done
		echo "deleted workspaces ${workspaces[*]}"
	fi
	left=$(managed_pods)
	[ -z "$left" ] && echo "no pods left" || echo "pods left: $left" >&2
	echo "state and manager log: $state"
}

# preflight: the namespace must hold no pod of another manager, which this one
# would take over.
preflight() {
	k auth can-i create pods >/dev/null || fail "the kubeconfig may not create pods in $NS"
	curl -fsS "$API/health" >/dev/null || fail "no Multica backend at $API"
	no_managed_pods || fail "namespace $NS already has managed pods (another manager?): $(managed_pods | tr '\n' ' ')"
	! curl -fsS -o /dev/null "$ADMIN/v1/inventory" 2>/dev/null || fail "something already listens on $ADMIN"
	echo "namespace $NS, kubeconfig $KUBECONFIG, backend $API, pods reach the manager at $manager_url, pods owned by ${owner:-nothing}, run $run"
}

# privacy: no pod name, IP or hostname of the run reaches Multica, in any
# workspace's runtimes, issues, comments, task runs or task messages.
privacy() {
	step "privacy: no pod name, hostname or IP in Multica"
	local out=$state/multica-dump w i t x names ips n
	: >"$out"
	for w in "${workspaces[@]}"; do
		ws=$w
		call GET /api/runtimes >>"$out"
		for i in $(call GET "/api/issues?limit=200" | jq -r '(.issues // .)[] | .id'); do
			call GET "/api/issues/$i" >>"$out"
			call GET "/api/issues/$i/comments" >>"$out"
			call GET "/api/issues/$i/task-runs" >>"$out"
			for t in $(call GET "/api/issues/$i/task-runs" | jq -r '.[].id'); do
				call GET "/api/tasks/$t/messages" >>"$out" 2>/dev/null || true
			done
		done
	done
	names=$(awk '{print $1}' "$state/pods-seen" | sort -u)
	ips=$(awk 'NF > 1 {print $2}' "$state/pods-seen" | sort -u)
	n=$(echo "$names" | grep -c . || true)
	[ "$n" -gt 0 ] || fail "no pods were seen"
	for x in $names $ips; do
		grep -qwF "$x" "$out" && fail "Multica shows $x"
	done
	grep -q 'sandbox-fake: agent ran in sandbox ' "$out" || fail "no agent comment names the neutral hostname"
	grep -oE 'agent ran in [^ ]+' "$out" | sort -u | grep -vx 'agent ran in sandbox' && fail "an agent ran on another hostname"
	pass "privacy: none of $n pod names or $(echo "$ips" | grep -c .) pod IPs appear in $(wc -c <"$out") bytes of Multica's runtimes, issues, comments, task runs and messages; agents saw the hostname 'sandbox'"
}
