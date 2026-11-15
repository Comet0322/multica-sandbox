#!/usr/bin/env bash
# Run the manager on the host against the dev namespace (up.sh first): it
# creates its pods in the namespace with the dev-user kubeconfig, which may
# only create, delete and watch pods, and serves Multica at 127.0.0.1:18080.
#
#   deploy/k8s/dev/manager.sh       sign in the bot, create a workspace, run
#
# The bot is pool-bot@dev.test; its PAT and workspace stay in .state, so a
# rerun serves the same workspace. Settings: PODS (SANDBOX_MAX_PODS, default
# 4), WARM (SANDBOX_WARM_PODS, default 1), and any SANDBOX_* variable.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
state=${STATE:-$here/.state}
api=http://127.0.0.1:18080
[ -f "$state/dev-user.kubeconfig" ] || { echo "run deploy/k8s/dev/up.sh first" >&2; exit 1; }
code=$(sed -n 's/^MULTICA_DEV_VERIFICATION_CODE=//p' "$state/backend.env")

if [ ! -s "$state/pool-token" ]; then
	curl -fsS -X POST "$api/auth/send-code" -H 'Content-Type: application/json' -d '{"email":"pool-bot@dev.test"}' >/dev/null
	jwt=$(curl -fsS -X POST "$api/auth/verify-code" -H 'Content-Type: application/json' \
		-d "{\"email\":\"pool-bot@dev.test\",\"code\":\"$code\"}" | jq -r .token)
	curl -fsS -X POST "$api/api/workspaces" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
		-d '{"name":"dev","slug":"dev"}' | jq -r .id >"$state/workspace"
	(umask 077 && curl -fsS -X POST "$api/api/tokens" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' \
		-d '{"name":"pool-manager"}' | jq -j .token >"$state/pool-token")
fi

# Pods reach the host at the kind network's gateway.
gw=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' | tr ' ' '\n' | grep -m1 '\.')
port=${SANDBOX_DEV_POD_PORT:-8792}
sed "s|@MANAGER_URL@|http://$gw:$port|" "$here/pod.yaml" >"$state/pod.yaml"

cd "$root"
go build -o "$state/sandbox-manager" ./cmd/sandbox-manager
export MULTICA_SERVER_URL=$api \
	SANDBOX_POOL_TOKEN_FILE="$state/pool-token" \
	SANDBOX_POD_TEMPLATE="$state/pod.yaml" \
	KUBECONFIG="$state/dev-user.kubeconfig" \
	SANDBOX_MAX_PODS=${PODS:-4} \
	SANDBOX_WARM_PODS=${WARM:-1} \
	SANDBOX_MANAGER_LISTEN=${SANDBOX_MANAGER_LISTEN:-0.0.0.0:$port} \
	SANDBOX_POOL_ADMIN_LISTEN=${SANDBOX_POOL_ADMIN_LISTEN:-127.0.0.1:8793} \
	SANDBOX_PLACEMENT=${SANDBOX_PLACEMENT:-task} \
	SANDBOX_HOLD_TIMEOUT=${SANDBOX_HOLD_TIMEOUT:-0} \
	SANDBOX_POD_CAPACITY=${SANDBOX_POD_CAPACITY:-1} \
	SANDBOX_AUDIT_FILE=${SANDBOX_AUDIT_FILE:-$state/audit.jsonl}
echo "manager: pods :$port (reached at $gw), admin http://127.0.0.1:8793/dispatch, workspace $(cat "$state/workspace")" >&2
exec "$state/sandbox-manager"
