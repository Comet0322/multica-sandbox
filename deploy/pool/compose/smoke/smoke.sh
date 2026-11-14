#!/bin/bash
# Compose smoke of the pool: a Multica backend with its own Postgres, the
# manager (stack.yaml) and one pod (pool.yaml), all in their own compose
# project, torn down with their volumes at the end.
#
# It checks that the pod registers through its entrypoint and presents its pod
# token, that a fake-agent task runs on it, that the pod is reset, and that it
# is lent again for a second task, which finds none of the first one's files.
# scenarios.sh runs the full scenario set.
#
# Build the images first, from the repository root:
#
#   docker build -f sandbox-manager/deploy/Dockerfile.manager -t multica-sandbox-manager:smoke .
#   docker build -f sandbox-manager/deploy/Dockerfile.pool --target fake -t multica-sandbox-pool:smoke-fake .
#
# Then: sandbox-manager/deploy/pool/compose/smoke/smoke.sh. Settings: see lib.sh.
set -euo pipefail

export SMOKE_PROJECT=${SMOKE_PROJECT:-pool-smoke}
SMOKE_OVERLAYS=""
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

start_backend
step "bot account, workspace and PAT"
bot_account pool-smoke
ws=${workspaces[0]}
echo "workspace $ws"

start_pool 1
pod_id=$pods
pod_host=$(inventory | jq -r --arg id "$pod_id" '.pods[] | select(.id == $id) | .name')
echo "pod $pod_id registered with label $pod_host"
dc exec -T pod jq -e '.token | startswith("mpt_")' /home/pod/.multica/config.json >/dev/null ||
	fail "the daemon config holds no pod token"
dc logs --no-log-prefix manager 2>&1 | grep -q "pod registered" || fail "the manager logged no registration"
status=$(dc exec -T pod curl -s -o /dev/null -w '%{http_code}' -X POST \
	http://sandbox-manager.multica-sandbox.svc.cluster.local:8080/api/daemon/heartbeat -d '{}')
[ "$status" = 401 ] || fail "a daemon call without a pod token got HTTP $status, want 401"
echo "a daemon call without a pod token is refused (401)"

rt=$(claude_runtime)
agent=$(make_agent fake-claude "$rt")

# run_task TITLE: assign an issue to the fake agent, wait for its task, and
# print the agent's comment.
run_task() {
	local issue
	issue=$(new_issue "$1" "$agent")
	wait_tasks "$issue" 1 completed 180 >/dev/null
	agent_comments "$issue" | grep . ||
		fail "the agent of '$1' left no comment (its mat_ calls did not get through)"
}

step "first task"
first=$(run_task "smoke task 1")
echo "$first"
resets() { audit | jq -s --arg id "$pod_id" '[.[] | select(.action == "reset" and .pod_id == $id and (.reason | startswith("reset completed")))] | length'; }
idle() { [ "$(pod_state "$pod_id")" = idle ]; }
reset_twice() { [ "$(resets)" -ge 2 ] && idle; }
wait_for "the reset after the first task" 120 reset_twice
echo "pod reset and idle again"

step "second task, lent again"
second=$(run_task "smoke task 2")
echo "$second"
case "$second" in *"earlier task files 0"*) ;; *) fail "the second task found the first one's files" ;; esac
used=$(leases | jq -r '[.[].pod_id] | unique | join(",")')
n=$(leases | jq 'length')
[ "$used" = "$pod_id" ] && [ "$n" -ge 2 ] || fail "want both leases on pod $pod_id, got $n on $used"

step "PASS: pod $pod_id registered, ran a task, reset and was lent again"
