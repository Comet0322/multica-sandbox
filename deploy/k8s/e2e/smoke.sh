#!/usr/bin/env bash
# A quick check of a namespace (a few minutes): the manager registers its pool
# and clean runtimes, one task of each kind runs on a real pod and completes,
# and no pod detail reaches Multica. Settings as for scenarios.sh (README.md).
#
#   deploy/k8s/e2e/smoke.sh
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

preflight
trap teardown EXIT
trap 'exit 130' INT TERM

step "bot account in one workspace"
bot_account a
ws=${workspaces[0]}
template "$image" >"$state/pod.yaml"
: >"$state/overrides.yaml"
step "manager"
start_manager
watch_pods &
watcher=$!
pool=$(make_agent smoke-pool "$(runtime pool)")
clean=$(make_agent smoke-clean "$(runtime clean)")

step "one pool task and one clean task"
p=$(new_issue "smoke pool" "$pool")
c=$(new_issue "smoke clean" "$clean")
tp=$(wait_tasks "$p" 1 completed 180)
tc=$(wait_tasks "$c" 1 completed 180)
pass "tasks: pool task ran on $(lease_pod "$tp"), clean task on $(lease_pod "$tc"); both completed"
stop_manager
privacy

step "smoke passed"
printf '  %s\n' "${passed[@]}"
