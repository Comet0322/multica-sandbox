#!/usr/bin/env bash
# Run deploy/k8s/e2e/smoke.sh or scenarios.sh against the kind namespace,
# keep the evidence, and check the run left nothing behind.
#
#   .claude/skills/verify-sandbox-manager/scripts/e2e.sh smoke|scenarios
#
# Evidence goes to $VERIFY_EVIDENCE/<UTC time>-<check>/ (default
# ${TMPDIR:-/tmp}/verify-sandbox-manager): run.log (the script's output),
# exit-code, head (commit and dirty files), leftovers, and state/ (the run's
# manager.log, audit.jsonl, pods-seen, multica-dump, pod template). The run's
# secrets (bot PAT, database URL, payload key) and the manager binary are
# removed from state/ afterwards. Exits with the script's code, or 1 if the
# run left a managed pod or a listener behind.
set -uo pipefail
check=${1:-}
case $check in smoke | scenarios) ;; *) echo "usage: $0 smoke|scenarios" >&2; exit 2 ;; esac
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
command -v kind >/dev/null || PATH=$here:$PATH
ev=${VERIFY_EVIDENCE:-${TMPDIR:-/tmp}/verify-sandbox-manager}/$(date -u +%Y%m%dT%H%M%SZ)-$check
mkdir -p "$ev"
{ git -C "$root" rev-parse HEAD; git -C "$root" status --porcelain; } >"$ev/head"
echo "evidence: $ev"

E2E_STATE=$ev/state "$root/deploy/k8s/e2e/$check.sh" 2>&1 | tee "$ev/run.log"
rc=${PIPESTATUS[0]}
echo "$rc" >"$ev/exit-code"
rm -f "$ev/state/pool-token" "$ev/state/database-url" "$ev/state/payload-key" "$ev/state/sandbox-manager"

kc=${E2E_KUBECONFIG:-$root/deploy/k8s/dev/.state/dev-user.kubeconfig}
pods=$(KUBECONFIG=$kc kubectl ${E2E_NAMESPACE:+-n "$E2E_NAMESPACE"} get pods -l app.kubernetes.io/managed-by=sandbox-manager -o name 2>&1)
listen=$(ss -ltnpH "( sport = :${E2E_POD_PORT:-8796} or sport = :${E2E_ADMIN_PORT:-8797} )" 2>/dev/null)
{ echo "managed pods: ${pods:-none}"; echo "listeners: ${listen:-none}"; } | tee "$ev/leftovers"
[ -z "$pods" ] && [ -z "$listen" ] || { echo "the run left something behind: see $ev/leftovers" >&2; [ "$rc" != 0 ] || rc=1; }
echo "$check exit $rc; evidence $ev"
exit "$rc"
