#!/usr/bin/env bash
# End-to-end scenarios against a namespace with real pods, the stock v0.6.4
# daemon (the fake-agent pod image) and a Multica v0.6.4 backend. The manager
# runs on this host; settings and how to point it at another namespace are in
# README.md.
#
#   handoff-across-pods  after the hold and the idle pod are gone, agent B
#                        lands on another pod and does not find agent A's
#                        unpushed branch
#   clean                a clean task gets a new pod, the pod serves its own
#                        issue again and no other, and a resume after the pod
#                        is gone runs on a new pod without an error
#   out-of-memory        a task over its pod's memory limit fails with the
#                        out-of-memory reason, and the next task runs
#   restart              restarting the manager mid-task keeps the task
#   handoff-on-one-pod   agent B lands on the held pod and finds agent A's
#                        unpushed branch
#   budget               a full budget deletes an idle pool runtime pod
#                        before a held one, and never a pod within another
#                        department's guarantee
#   owner                every pod is owned by the owner Deployment, and
#                        deleting it has Kubernetes delete the pods (kind
#                        only)
#   cannot-start         a pod whose image is missing breaks the pods at once:
#                        one pod at a time tries, the task stays queued,
#                        /readyz says why; once the image is there (kind
#                        only), pods start again and the task completes
#   privacy              no pod name, hostname or IP appears in Multica
#
#   deploy/k8s/e2e/scenarios.sh
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

preflight
trap teardown EXIT
trap 'exit 130' INT TERM

step "bot account in two workspaces (departments a and b)"
bot_account a b
wsa=${workspaces[0]} wsb=${workspaces[1]}
template "$image" >"$state/pod.yaml"
missing_image=multica-sandbox-pool:e2e-missing-$run
template "$missing_image" >"$state/pod-broken.yaml"
overrides 'a: {hold_timeout: 20s}'

step "manager: pool and clean runtimes, 4 pods, 1 warm, idle pods kept 10s"
start_manager SANDBOX_POD_MAX_IDLE=10s
watch_pods &
watcher=$!
ws=$wsa
rta=$(runtime pool)
rtca=$(runtime clean)
ws=$wsb
rtb=$(runtime pool)
ws=$wsa
agent_a=$(make_agent agent-a "$rta" '{"SANDBOX_FAKE_BRANCH":"handoff"}')
agent_b=$(make_agent agent-b "$rta" '{"SANDBOX_FAKE_BRANCH":"handoff"}')
clean=$(make_agent clean "$rtca")
oom=$(make_agent oom "$rta" '{"SANDBOX_FAKE_MEMORY":"1536"}')
slow=$(make_agent slow "$rta" '{"SANDBOX_FAKE_SLEEP":"45"}')
fast=$(make_agent fast "$rta")
ws=$wsb
fast_b=$(make_agent fast-b "$rtb")
slow_b=$(make_agent slow-b "$rtb" '{"SANDBOX_FAKE_SLEEP":"20"}')

# --- handoff across pods -------------------------------------------------------
step "handoff across pods (hold 20s, idle 10s)"
ws=$wsa
y=$(new_issue "handoff across pods" "$agent_a")
t1=$(wait_tasks "$y" 1 completed 180)
pa=$(lease_pod "$t1")
last_comment "$y" | grep -q 'branch handoff created' || fail "agent A did not create its branch: $(last_comment "$y")"
wait_for "agent A's pod $pa to be deleted after its hold and idle time" 120 pod_gone "$pa"
assign "$y" "$agent_b"
t2=$(wait_tasks "$y" 2 completed 180)
pb=$(lease_pod "$t2")
[ "$pb" != "$pa" ] || fail "agent B ran on agent A's pod $pa"
c=$(last_comment "$y")
case "$c" in *"earlier task files 0"*"branch handoff created"*) ;; *) fail "agent B found something of agent A's on $pb: $c" ;; esac
pass "handoff across pods: A ran on $pa, which was deleted after its hold; B ran on $pb, found no file and no branch of A's (only what A pushed would be there)"

# --- clean ----------------------------------------------------------------------
step "clean runtime (hold 20s)"
seen_before=$(leases | jq -r '.[].pod_id' | sort -u)
z=$(new_issue "clean z" "$clean")
tz1=$(wait_tasks "$z" 1 completed 180)
pc=$(lease_pod "$tz1")
id=$(leases | jq -r --arg t "$tz1" '[.[] | select(.task_id == $t)][0].pod_id')
grep -qxF "$id" <<<"$seen_before" && fail "the clean task ran on $pc, which had served before"
last_comment "$z" | grep -q 'earlier task files 0' || fail "the clean task's pod had files: $(last_comment "$z")"
wait_for "$pc to be held" 30 pod_in_phase "$pc" held
comment "$z" "again"
w=$(new_issue "clean w" "$clean")
tz2=$(wait_tasks "$z" 2 completed 120)
[ "$(lease_pod "$tz2")" = "$pc" ] || fail "the issue's second clean task ran on $(lease_pod "$tz2"), want its held pod $pc"
case "$(last_comment "$z")" in *"earlier task files 0"*) fail "the second task did not find the first one's files on $pc" ;; esac
tw=$(wait_tasks "$w" 1 completed 120)
pw=$(lease_pod "$tw")
[ "$pw" != "$pc" ] || fail "another issue's clean task ran on $pc"
wait_for "the clean pod $pc to be deleted when its hold ends" 90 pod_gone "$pc"
comment "$z" "resume"
tz3=$(wait_tasks "$z" 3 completed 180)
pz3=$(lease_pod "$tz3")
[ "$pz3" != "$pc" ] || fail "the resumed task ran on the deleted pod"
last_comment "$z" | grep -q 'earlier task files 0' || fail "the resumed task's new pod had files"
pass "clean: issue z got the new pod $pc and again within its hold; issue w got $pw; after the hold the resume ran on the new pod $pz3 and completed"

# --- out of memory -------------------------------------------------------------
step "out of memory (a 1.5 GiB task in a 1Gi pod)"
o=$(new_issue "out of memory" "$oom")
wait_tasks "$o" 1 failed 180 >/dev/null
err=$(task_error "$o" 1)
case "$err" in *"ran out of memory"*) ;; *) fail "the task failed with '$err', want the out-of-memory reason" ;; esac
# Stop Multica retrying it.
ws=$wsa call POST "/api/agents/$oom/archive" >/dev/null
nx=$(new_issue "after out of memory" "$fast")
wait_tasks "$nx" 1 completed 180 >/dev/null
pass "out of memory: the task failed with '$err'; the next task completed"

# --- restart -------------------------------------------------------------------
step "restart the manager mid-task"
r=$(new_issue "restart" "$slow")
tr=$(wait_tasks "$r" 1 running 120)
sleep 5
stop_manager
echo "manager stopped while task $tr runs"
overrides 'a: {hold_timeout: 10m}'
start_manager
grep -q 'pods restored' "$state/manager.log" || fail "the new manager restored no pods"
wait_tasks "$r" 1 completed 120 >/dev/null
pass "restart: task $tr ran on through a manager restart and completed"

# --- handoff on one pod --------------------------------------------------------
step "handoff on one pod (hold 10m)"
x=$(new_issue "handoff on one pod" "$agent_a")
tx1=$(wait_tasks "$x" 1 completed 180)
px=$(lease_pod "$tx1")
last_comment "$x" | grep -q 'branch handoff created' || fail "agent A did not create its branch: $(last_comment "$x")"
wait_for "$px to be held" 30 pod_in_phase "$px" held
assign "$x" "$agent_b"
tx2=$(wait_tasks "$x" 2 completed 180)
[ "$(lease_pod "$tx2")" = "$px" ] || fail "agent B ran on $(lease_pod "$tx2"), want the held pod $px"
last_comment "$x" | grep -q 'branch handoff found' || fail "agent B did not find A's branch: $(last_comment "$x")"
pass "handoff on one pod: agent B landed on the held pod $px and found agent A's unpushed branch"

# --- budget --------------------------------------------------------------------
step "budget: 3 pods"
stop_manager
k delete pods -l "$LABEL" --wait=true >/dev/null
overrides 'a: {hold_timeout: 10m}
b: {hold_timeout: 10m, guarantee: 1}'
start_manager SANDBOX_MAX_PODS=3
ws=$wsb
b1=$(new_issue "budget b1" "$fast_b")
pb1=$(lease_pod "$(wait_tasks "$b1" 1 completed 180)")
ws=$wsa
a0=$(new_issue "budget a0" "$slow")
wait_tasks "$a0" 1 running 120 >/dev/null
a1=$(new_issue "budget a1" "$fast")
pa1=$(lease_pod "$(wait_tasks "$a1" 1 completed 180)")
wait_for "$pa1 to be held" 30 pod_in_phase "$pa1" held
# a0 ends with no hold, so its pod becomes idle.
overrides 'a: {hold_timeout: 0s}
b: {hold_timeout: 10m, guarantee: 1}'
ta0=$(wait_tasks "$a0" 1 completed 120)
pa0=$(lease_pod "$ta0")
wait_for "$pa0 to be idle" 30 pod_in_phase "$pa0" idle
echo "pods: $pb1 held (b), $pa1 held (a), $pa0 idle (a)"
ws=$wsb
b2=$(new_issue "budget b2" "$fast_b")
pb2=$(lease_pod "$(wait_tasks "$b2" 1 completed 180)")
pod_gone "$pa0" || fail "the idle pod $pa0 was kept"
pod_exists "$pa1" || fail "the held pod $pa1 was deleted before the idle one"
pod_exists "$pb1" || fail "b's held pod $pb1 was deleted"
echo "b2 ran on $pb2; the idle pod $pa0 was deleted, the held ones kept"
overrides 'a: {hold_timeout: 10m}
b: {hold_timeout: 10m, guarantee: 2}'
wait_for "$pb2 to be held" 30 pod_in_phase "$pb2" held
ws=$wsa
a2=$(new_issue "budget a2" "$fast")
pa2=$(lease_pod "$(wait_tasks "$a2" 1 completed 180)")
pod_exists "$pb1" && pod_exists "$pb2" || fail "a pod within b's guarantee was deleted for a"
pod_gone "$pa1" || fail "a's own held pod $pa1 was kept, so something else went"
pass "budget: with 3 pods full, b's task deleted a's idle pod $pa0 and kept the held $pa1; with b's guarantee 2, a's task deleted a's own held $pa1 and kept b's $pb1 and $pb2"

# --- owner ----------------------------------------------------------------------
if [ -n "$owner" ]; then
	step "owner: the pods belong to deployment $owner"
	uid=$(k get deployment "$owner" -o jsonpath='{.metadata.uid}')
	pods=$(managed_pods)
	[ -n "$pods" ] || fail "no pod to check"
	for p in $pods; do
		o=$(k get pod "$p" -o jsonpath='{range .metadata.ownerReferences[*]}{.kind}/{.name}/{.uid}/{.blockOwnerDeletion}{"\n"}{end}')
		[ "$o" = "Deployment/$owner/$uid/false" ] || fail "pod $p is owned by '$o', want Deployment/$owner/$uid/false"
	done
	n=$(wc -w <<<"$pods")
	# Only the dev stand-in is deleted, and put back from deploy/k8s/dev; the
	# kubeconfig may not delete deployments, so the kind admin context does.
	adminctl() { KUBECONFIG=${E2E_ADMIN_KUBECONFIG:-$HOME/.kube/config} kubectl "$@"; }
	admin=${E2E_ADMIN_CONTEXT:-}
	if [ -z "$admin" ] && adminctl config get-contexts -o name 2>/dev/null | grep -qx kind-sandbox-dev; then admin=kind-sandbox-dev; fi
	if [ "$owner" = sandbox-e2e-owner ] && [ -n "$admin" ]; then
		# Deleting the manager's Deployment stops the manager too.
		stop_manager
		adminctl --context "$admin" -n "$NS" delete deployment "$owner" --wait=true >/dev/null
		wait_for "Kubernetes to delete the $n pods the deployment owned" 90 no_managed_pods
		adminctl --context "$admin" -n "$NS" apply -f "$root/deploy/k8s/dev/owner.yaml" >/dev/null
		deleted="; deleting the deployment had Kubernetes delete all $n pods"
	else
		deleted="; deletion not checked (only the kind stand-in sandbox-e2e-owner is deleted)"
	fi
	pass "owner: all $n pods are owned by deployment $owner (not blocking its deletion)$deleted"
fi

# --- a pod that cannot start ---------------------------------------------------
step "a pod that cannot start (its image is missing)"
stop_manager
k delete pods -l "$LABEL" --wait=true >/dev/null
cp "$state/pod-broken.yaml" "$state/pod.yaml"
start_manager SANDBOX_POD_START_TIMEOUT=20s SANDBOX_WARM_PODS=1
ws=$wsa
nostart=$(make_agent nostart "$rta")
n=$(new_issue "cannot start" "$nostart")
wait_for "the pods to break" 60 sh -c "curl -fsS '$ADMIN/v1/inventory' | jq -e '.broken // empty' >/dev/null"
inv=$(inventory)
broken=$(jq -r .broken <<<"$inv")
case "$broken" in *"image cannot be pulled"*"Check the image in the pod template"*) ;; *) fail "broken = '$broken', want the image and what to check" ;; esac
jq -e '.failures[0] | .kind == "image" and .phase == "start" and .permanent' <<<"$inv" >/dev/null ||
	fail "latest failure $(jq -c '.failures[0]' <<<"$inv"), want a permanent image start failure"
ready=$(curl -sS -o "$state/readyz" -w '%{http_code}' "http://127.0.0.1:$pod_port/readyz")
[ "$ready" = 503 ] && grep -q 'pods do not start' "$state/readyz" || fail "/readyz = $ready $(cat "$state/readyz"), want 503: pods do not start"
most=0
for _ in $(seq 20); do
	c=$(managed_pods | grep -c . || true)
	[ "$c" -gt "$most" ] && most=$c
	sleep 2
done
[ "$most" -le 1 ] || fail "$most pods at once while broken, want one at a time"
st=$(tasks "$n" | jq -rs '.[0].status')
[ "$st" = queued ] || fail "the task is $st while no pod can start, want queued"
echo "broken: $broken; at most $most pod at a time for 40s; the task queued; /readyz 503"
cluster=${E2E_KIND_CLUSTER:-}
if [ -z "$cluster" ] && command -v kind >/dev/null && kind get clusters 2>/dev/null | grep -qx sandbox-dev; then cluster=sandbox-dev; fi
if [ -n "$cluster" ]; then
	docker tag "$image" "$missing_image"
	kind load docker-image --name "$cluster" "$missing_image" >/dev/null
	wait_for "pods to start again" 330 sh -c "curl -fsS '$ADMIN/v1/inventory' | jq -e '(.broken // \"\") == \"\"' >/dev/null"
	wait_tasks "$n" 1 completed 180 >/dev/null
	ready=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$pod_port/readyz")
	[ "$ready" = 200 ] || fail "/readyz = $ready after pods started again, want 200"
	recovered="; once the image was loaded, pods started again without a restart and the task completed"
else
	recovered="; recovery not checked (no kind cluster to load the image into)"
fi
ws=$wsa call POST "/api/agents/$nostart/archive" >/dev/null
pass "cannot start: a missing image broke the pods at once, one pod at a time tried, the task stayed queued and /readyz said why$recovered"
stop_manager
k delete pods -l "$LABEL" --wait=true >/dev/null
if [ -n "$cluster" ]; then
	# Remove the tag only: the image is the pod image under another name, and
	# removing it by ID (crictl rmi) would remove that too.
	for node in $(kind get nodes --name "$cluster"); do
		docker exec "$node" ctr -n k8s.io images rm "docker.io/library/$missing_image" >/dev/null 2>&1 || true
	done
	docker rmi "$missing_image" >/dev/null 2>&1 || true
fi
template "$image" >"$state/pod.yaml"

privacy

step "all scenarios passed"
printf '  %s\n' "${passed[@]}"
