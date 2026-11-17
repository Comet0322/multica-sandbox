#!/usr/bin/env bash
# Create (or reuse) a kind cluster with one namespace holding Multica v0.6.4,
# and a kubeconfig that may only create, delete, watch and patch pods there,
# what the manager needs.
#
#   deploy/k8s/dev/up.sh          create or update
#   deploy/k8s/dev/down.sh        delete the cluster
#
# Settings (environment):
#   CLUSTER    kind cluster name           (default sandbox-dev)
#   NAMESPACE  the one namespace           (default multica-sandbox)
#   STATE      where secrets and the kubeconfig go (default deploy/k8s/dev/.state)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
cluster=${CLUSTER:-sandbox-dev}
ns=${NAMESPACE:-multica-sandbox}
state=${STATE:-$here/.state}
ctx=kind-$cluster
k() { kubectl --context "$ctx" "$@"; }

mkdir -p "$state" && chmod 700 "$state"

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	kind create cluster --name "$cluster" --config "$here/kind.yaml"
fi

# Load the images from the host's Docker when it has them, so the node does
# not pull them again.
for img in ghcr.io/multica-ai/multica-backend:v0.6.4 ghcr.io/multica-ai/multica-web:v0.6.4 pgvector/pgvector:pg17; do
	if docker image inspect "$img" >/dev/null 2>&1; then
		kind load docker-image --name "$cluster" "$img" >/dev/null
	fi
done

k create namespace "$ns" --dry-run=client -o yaml | k apply -f -
# Your namespace's policy is not known here; baseline is enforced and
# restricted only warned about.
k label namespace "$ns" --overwrite \
	pod-security.kubernetes.io/enforce=baseline \
	pod-security.kubernetes.io/warn=restricted >/dev/null

# Secrets are made once and kept, so the database survives a rerun.
if [ ! -f "$state/backend.env" ]; then
	pw=$(openssl rand -hex 16)
	{
		echo "POSTGRES_PASSWORD=$pw"
		echo "DATABASE_URL=postgres://multica:$pw@multica-pg:5432/multica?sslmode=disable"
		echo "JWT_SECRET=$(openssl rand -hex 32)"
		echo "MULTICA_DEV_VERIFICATION_CODE=$(shuf -i 100000-999999 -n1)"
	} >"$state/backend.env"
	chmod 600 "$state/backend.env"
fi
k -n "$ns" create secret generic multica-backend --from-env-file="$state/backend.env" \
	--dry-run=client -o yaml | k apply -f -
k -n "$ns" create configmap multica-backend \
	--from-literal=APP_ENV=development \
	--from-literal=FRONTEND_ORIGIN=http://localhost:13001 \
	--from-literal=MULTICA_APP_URL=http://localhost:13001 \
	--from-literal=MULTICA_PUBLIC_URL=http://localhost:18080 \
	--dry-run=client -o yaml | k apply -f -

k -n "$ns" apply -k "$here"
k -n "$ns" apply -f "$here/dev-user.yaml" -f "$here/owner.yaml"

k -n "$ns" rollout status statefulset/multica-pg --timeout=180s
k -n "$ns" rollout status deployment/multica-backend --timeout=180s
k -n "$ns" rollout status deployment/multica-web --timeout=180s

# The dev user's kubeconfig: the cluster's address as the host sees it, and
# the service account's long-lived token.
token=""
for _ in $(seq 30); do
	token=$(k -n "$ns" get secret dev-user-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
	[ -n "$token" ] && break
	sleep 1
done
[ -n "$token" ] || { echo "the dev-user token was not issued" >&2; exit 1; }
server=$(k config view --minify -o jsonpath='{.clusters[0].cluster.server}')
k config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$state/ca.crt"
kc="$state/dev-user.kubeconfig"
rm -f "$kc"
KUBECONFIG=$kc kubectl config set-cluster "$cluster" --server="$server" \
	--certificate-authority="$state/ca.crt" --embed-certs=true >/dev/null
KUBECONFIG=$kc kubectl config set-credentials dev-user --token="$token" >/dev/null
KUBECONFIG=$kc kubectl config set-context dev-user --cluster="$cluster" --user=dev-user --namespace="$ns" >/dev/null
KUBECONFIG=$kc kubectl config use-context dev-user >/dev/null
chmod 600 "$kc"

code=$(sed -n 's/^MULTICA_DEV_VERIFICATION_CODE=//p' "$state/backend.env")
cat <<MSG

Namespace $ns is ready in kind cluster $cluster.
  backend     http://127.0.0.1:18080   (curl http://127.0.0.1:18080/health)
  web         http://127.0.0.1:13001   (sign in with any email, code $code)
  kubeconfig  $kc   (pods only: create, delete, get, list, watch, patch)
MSG
