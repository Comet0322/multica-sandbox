#!/bin/bash
# End-to-end scenarios of the pool against a real Multica backend (by default
# the v0.6.4 image) with its own Postgres: one bot account in two workspaces,
# the manager and two fake-agent pods of one slot each, all in their own
# compose project, torn down with their volumes at the end.
#
#   1 registration      pods register through their entrypoint and present
#                        their pod token; calls without one are refused
#   2 identity          one daemon identity and one runtime per workspace;
#                        no pod detail reaches the server
#   3 reset             a reset deletes the task's files and keeps the skip
#                        directories (.multica with the pod token, .claude/skills)
#   4 queue             claims follow priority, then age, across workspaces
#   5 quota             a workspace at its quota is skipped for the next one
#   6 hold              the same person's next task on the same issue reuses
#                        the held pod, renews the hold, and the hold expires
#   7 eviction          with no idle pod, the hold idle longest is evicted for
#                        a task of another affinity key; the other hold stays
#   8 lifetime          a binding is reset at its maximum lifetime, and a task
#                        running past it and the drain grace is failed
#
# Images as for smoke.sh; settings in lib.sh. Run:
#   deploy/pool/compose/smoke/scenarios.sh
set -euo pipefail

export SMOKE_PROJECT=${SMOKE_PROJECT:-pool-scenarios}
SMOKE_OVERLAYS="scenarios.yaml"
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

manager_url=http://sandbox-manager.multica-sandbox.svc.cluster.local:8080
passed=()
pass() {
	passed+=("$1")
	echo "PASS: $1"
}

# overrides YAML HOLD_A HOLD_B [QUOTA_A]: write the overrides file and wait
# until the manager applies it (hold timeouts as the inventory prints them,
# e.g. 30s or 10m0s; quota 0 is no cap).
overrides() {
	printf '%s\n' "$1" >"$state/overrides/overrides.yaml"
	want_a=$2 want_b=$3 want_quota=${4:-0}
	applied() {
		inventory | jq -e --arg a "$want_a" --arg b "$want_b" --argjson q "$want_quota" '
			([.workspaces[] | select(.slug == "pool-a")][0] | .hold_timeout == $a and .quota == $q) and
			([.workspaces[] | select(.slug == "pool-b")][0].hold_timeout == $b) and
			((.override_problems // []) | length == 0)' >/dev/null
	}
	wait_for "the overrides to apply" 30 applied
}

all_idle() { inventory | jq -e '[.pods[] | select(.state != "idle")] | length == 0' >/dev/null; }

# last_comment ISSUE prints the fake agent's latest comment on ISSUE.
last_comment() { agent_comments "$1" | tail -n 1; }

start_backend
step "bot account in two workspaces"
bot_account pool-a pool-b
wsa=${workspaces[0]} wsb=${workspaces[1]}
echo "workspaces pool-a $wsa, pool-b $wsb"

# --- 1 registration ---------------------------------------------------------
start_pool 2
step "1 registration through the entrypoint"
read -r p1 p2 <<<"$pods"
tokens=()
for i in 1 2; do
	host=$(dc exec -T --index "$i" pod hostname | tr -d '\r')
	inventory | jq -e --arg n "$host" '[.pods[] | select(.name == $n)] | length == 1' >/dev/null ||
		fail "no pod is labelled with container $i's hostname $host"
	tok=$(dc exec -T --index "$i" pod jq -r .token /home/pod/.multica/config.json | tr -d '\r')
	case "$tok" in mpt_*) ;; *) fail "container $i's daemon config holds no pod token" ;; esac
	tokens+=("$tok")
done
[ "${tokens[0]}" != "${tokens[1]}" ] || fail "the two pods got the same token"
for auth in "" "Authorization: Bearer mpt_0000" "Authorization: Bearer mul_not_a_pod"; do
	status=$(dc exec -T --index 1 pod curl -s -o /dev/null -w '%{http_code}' ${auth:+-H "$auth"} \
		-X POST "$manager_url/api/daemon/heartbeat" -d '{}')
	[ "$status" = 401 ] || fail "a daemon call with '${auth:-no token}' got HTTP $status, want 401"
done
pass "registration: pods $p1 and $p2 registered with their hostnames, distinct mpt_ tokens; no or forged tokens get 401"

# --- 2 identity -------------------------------------------------------------
step "2 one daemon identity across both workspaces"
ws=$wsa rta=$(claude_runtime)
ws=$wsb rtb=$(claude_runtime)
ids=""
for w in "$wsa" "$wsb"; do
	ws=$w
	rts=$(call GET /api/runtimes)
	[ "$(jq '[.[] | select(.daemon_id != null)] | length' <<<"$rts")" = 1 ] ||
		fail "workspace $w has $(jq length <<<"$rts") runtimes, want the manager's one"
	ids+="$(jq -r '.[0].daemon_id' <<<"$rts") "
	for p in $p1 $p2; do
		name=$(inventory | jq -r --arg p "$p" '.pods[] | select(.id == $p) | .name')
		grep -qF -e "$p" -e "$name" <<<"$rts" && fail "workspace $w's runtime names pod $p ($name)"
	done
done
read -r ida idb <<<"$ids"
[ "$ida" = "$idb" ] || fail "daemon IDs differ: $ida in pool-a, $idb in pool-b"
case "$ida" in sandbox-manager-*) ;; *) fail "daemon ID $ida is not the manager's derived identity" ;; esac
pass "identity: one runtime per workspace, both under daemon $ida; no pod name or ID in them"

ws=$wsa
fasta=$(make_agent fast-a "$rta")
slowa=$(make_agent slow-a "$rta" 20)
ws=$wsb
fastb=$(make_agent fast-b "$rtb")
slowb=$(make_agent slow-b "$rtb" 20)
longb=$(make_agent long-b "$rtb" 60)

# --- 3 reset keeps the skip directories --------------------------------------
step "3 reset keeps the skip directories"
ws=$wsa
issue=$(new_issue "reset" "$fasta")
task=$(wait_tasks "$issue" 1 completed 120)
pod=$(lease_of "$task")
[ -n "$pod" ] || fail "no lease for task $task"
# The first reset is the one at registration.
reset_done() { [ "$(resets_of "$pod" | wc -l)" -ge 2 ] && [ "$(pod_state "$pod")" = idle ]; }
wait_for "pod $pod to be reset and idle" 60 reset_done
resets_of "$pod" | tail -n 1 | sed 's/^/  reset: /'
i=$(pod_container "$pod")
home=$(dc exec -T --index "$i" pod sh -c 'cd /home/pod && find . -mindepth 1 -maxdepth 3 | sort' | tr -d '\r')
echo "$home" | sed 's/^/  /'
grep -q 'sandbox-fake-left' <<<"$home" && fail "the task's files survived the reset"
grep -q '^./workspaces' <<<"$home" && fail "the task directory survived the reset"
grep -q '^./.claude/projects' <<<"$home" && fail ".claude/projects survived the reset"
grep -qx './.claude/skills/sandbox-environment' <<<"$home" || fail "the image skills did not survive the reset"
grep -qx './.multica/config.json' <<<"$home" || fail ".multica/config.json did not survive the reset"
tok=$(dc exec -T --index "$i" pod jq -r .token /home/pod/.multica/config.json | tr -d '\r')
[ "$tok" = "${tokens[$((i - 1))]}" ] || fail "the pod token changed across the reset"
last_comment "$issue" | grep -q 'with skill hidden-runtime-skill sandbox-environment' ||
	fail "the agent did not see the image skills: $(last_comment "$issue")"
pass "reset: task files, workspaces and .claude/projects deleted; .multica/config.json (same token) and .claude/skills kept"

# --- 4 global queue order ----------------------------------------------------
step "4 global queue: priority, then age, across workspaces"
ws=$wsa
b1=$(new_issue "blocker 1" "$slowa")
wait_tasks "$b1" 1 running 60 >/dev/null
sleep 6
b2=$(new_issue "blocker 2" "$slowa")
wait_tasks "$b2" 1 running 60 >/dev/null
ws=$wsa alow=$(new_issue "a low" "$fasta" low)
sleep 1
ws=$wsa ahigh=$(new_issue "a high" "$fasta" high)
sleep 1
ws=$wsb burgent=$(new_issue "b urgent" "$fastb" urgent)
sleep 1
ws=$wsb bhigh=$(new_issue "b high" "$fastb" high)
order=()
for spec in "$wsb:$burgent" "$wsa:$ahigh" "$wsb:$bhigh" "$wsa:$alow"; do
	ws=${spec%%:*}
	t=$(wait_tasks "${spec#*:}" 1 completed 180)
	order+=("$(lease_time "$t") ${spec#*:}")
done
want="$burgent $ahigh $bhigh $alow"
got=$(printf '%s\n' "${order[@]}" | sort -n | awk '{print $2}' | xargs)
[ "$got" = "$want" ] || fail "lease order $got, want b-urgent a-high b-high a-low ($want)"
pass "queue: queued while both pods were busy, leased b-urgent, a-high, b-high, a-low (created a-low, a-high, b-urgent, b-high)"
wait_for "both pods idle" 120 all_idle

# --- 5 quota -----------------------------------------------------------------
step "5 quota: a workspace at its quota is skipped"
overrides 'pool-a: {quota: 1}' 0s 0s 1
ws=$wsa
a1=$(new_issue "quota a1" "$slowa")
a2=$(new_issue "quota a2" "$slowa")
ta1=$(wait_tasks "$a1" 1 running 60)
sleep 6
st=$(tasks "$a2" | jq -rs '.[0].status')
[ "$st" = queued ] || fail "pool-a's second task is $st with quota 1, want queued"
inventory | jq -e '[.pods[] | select(.state == "idle")] | length == 1' >/dev/null ||
	fail "want one pod idle while pool-a is at its quota"
echo "pool-a's second task stays queued with a pod idle"
ws=$wsb
b=$(new_issue "quota b" "$slowb")
tb=$(wait_tasks "$b" 1 running 60)
ws=$wsa
ta2=$(wait_tasks "$a2" 1 completed 120)
ws=$wsa wait_tasks "$a1" 1 completed 60 >/dev/null
[ "$(lease_time "$tb")" -lt "$(lease_time "$ta2")" ] || fail "pool-b's later task was not leased before pool-a's second"
ta1_end=$(ns "$(leases | jq -r --arg t "$ta1" '.[] | select(.task_id == $t) | .ended_at')")
[ "$ta1_end" -le "$(lease_time "$ta2")" ] || fail "pool-a's second task leased before its first ended"
pass "quota: with pool-a at quota 1 its second task waited (a pod idle), pool-b's later task ran first"
ws=$wsb wait_tasks "$b" 1 completed 60 >/dev/null
wait_for "both pods idle" 60 all_idle

# --- 6 hold: affinity reuse, renewal, expiry ---------------------------------
step "6 hold: the same issue's next task reuses the held pod"
overrides 'pool-a: {hold_timeout: 30s}' 30s 0s
ws=$wsa
x=$(new_issue "hold x" "$fasta")
t1=$(wait_tasks "$x" 1 completed 120)
hp=$(lease_of "$t1")
held() { binding_of "$hp" | jq -e '.hold == true' >/dev/null; }
wait_for "pod $hp to hold" 30 held
h1=$(binding_of "$hp" | jq -r .hold_until)
key=$(binding_of "$hp" | jq -r .affinity_key)
echo "pod $hp holds $key until $h1"
resets_before=$(resets_of "$hp" | wc -l)
comment "$x" "again"
t2=$(wait_tasks "$x" 2 completed 120)
[ "$(lease_of "$t2")" = "$hp" ] || fail "the issue's next task ran on $(lease_of "$t2"), want the held pod $hp"
[ "$(resets_of "$hp" | wc -l)" = "$resets_before" ] || fail "pod $hp was reset between the two tasks of one binding"
c2=$(last_comment "$x")
case "$c2" in *"earlier task files "[1-9]*) ;; *) fail "the second task did not find the first one's files: ${c2:-no comment}" ;; esac
wait_for "pod $hp to hold again" 30 held
h2=$(binding_of "$hp" | jq -r .hold_until)
[ "$(ns "$h2")" -gt "$(ns "$h1")" ] || fail "the hold was not renewed: $h1 then $h2"
echo "renewed: hold until $h2 (was $h1); the second task found the first one's files"
expired() { resets_of "$hp" | grep -qx 'hold expired' && [ "$(pod_state "$hp")" = idle ]; }
wait_for "the hold to expire" 60 expired
pass "hold: $key reused pod $hp without a reset, renewed its hold ($h1 -> $h2), then reset on 'hold expired'"

# --- 7 eviction ---------------------------------------------------------------
step "7 eviction: the hold idle longest goes to a task of another key"
overrides 'pool-a: {hold_timeout: 45s}' 45s 0s
ws=$wsa
e1=$(new_issue "evict 1" "$fasta")
te1=$(wait_tasks "$e1" 1 completed 120)
ep1=$(lease_of "$te1")
hp=$ep1 wait_for "pod $ep1 to hold" 30 held
sleep 2
e2=$(new_issue "evict 2" "$fasta")
te2=$(wait_tasks "$e2" 1 completed 120)
ep2=$(lease_of "$te2")
[ "$ep2" != "$ep1" ] || fail "the second key's task went to the held pod while $ep2 was idle"
hp=$ep2 wait_for "pod $ep2 to hold" 30 held
key2=$(binding_of "$ep2" | jq -r .affinity_key)
e3=$(new_issue "evict 3" "$fasta")
te3=$(wait_tasks "$e3" 1 completed 120)
[ "$(lease_of "$te3")" = "$ep1" ] || fail "the third key's task ran on $(lease_of "$te3"), want the hold idle longest $ep1"
resets_of "$ep1" | grep -qx "evicted for task $te3 of another affinity key" || fail "no eviction reset of $ep1 for $te3"
resets_of "$ep2" | grep -q 'evicted' && fail "pod $ep2 was evicted too"
binding_of "$ep2" | jq -e --arg k "$key2" '.affinity_key == $k' >/dev/null || fail "pod $ep2 lost its binding $key2"
pass "eviction: with both pods held, $ep1 (held longest) was evicted for task $te3; $ep2 kept $key2"
wait_for "the holds to expire" 120 all_idle

# --- 8 maximum lifetime -------------------------------------------------------
step "8 maximum lifetime: reset in hold, then a task failed after the drain grace"
overrides 'pool-b: {hold_timeout: 10m, max_lifetime: 15s}' 0s 10m0s
ws=$wsb
l1=$(new_issue "lifetime 1" "$fastb")
tl1=$(wait_tasks "$l1" 1 completed 120)
lp=$(lease_of "$tl1")
hp=$lp wait_for "pod $lp to hold" 30 held
reached() { resets_of "$lp" | grep -qx 'maximum lifetime reached' && [ "$(pod_state "$lp")" = idle ]; }
wait_for "the binding's maximum lifetime" 60 reached
echo "pod $lp reset in hold at its maximum lifetime (hold timeout 10m)"
l2=$(new_issue "lifetime 2" "$longb")
tl2=$(wait_tasks "$l2" 1 running 60)
lp2=$(lease_of "$tl2")
wait_tasks "$l2" 1 failed 90 >/dev/null
err=$(tasks "$l2" | jq -rs '.[0].error // ""')
case "$err" in *"maximum lifetime"*) ;; *) fail "task $tl2 failed with '$err', want the maximum lifetime reason" ;; esac
graced() { resets_of "$lp2" | grep -qx 'maximum lifetime and drain grace passed'; }
wait_for "the drain grace reset" 30 graced
pass "lifetime: hold reset at 15s ('maximum lifetime reached'); task $tl2 (60s) failed upstream after 15s + 10s grace and pod $lp2 reset"

step "upstream calls"
# The backend logs one "http request" line per call. Every call the manager
# made, or relayed for a pod or an agent, must have succeeded.
calls=$(dc logs --no-log-prefix backend 2>/dev/null | grep 'http request' |
	sed -E 's/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/ID/g' |
	grep -oE 'method=[A-Z]+ path=[^ ]+ status=[0-9]+' | sort | uniq -c | sort -rn)
echo "$calls" | sed 's/^/  /'
bad=$(echo "$calls" | grep -E 'status=[45][0-9][0-9]' || true)
[ -z "$bad" ] || fail "the backend answered calls with an error: $bad"
pass "upstream: every call to $MULTICA_BACKEND_IMAGE succeeded"

step "all scenarios passed"
printf '  %s\n' "${passed[@]}"
