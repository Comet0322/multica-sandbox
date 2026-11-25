#!/usr/bin/env bash
# Read-only: is the kind namespace worth driving? Prints one line per check
# (ok, warn, FAIL) and exits 1 on any FAIL. Changes nothing.
#
#   .claude/skills/verify-sandbox-manager/scripts/doctor.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
cluster=${CLUSTER:-sandbox-dev}
ns=${NAMESPACE:-multica-sandbox}
ctx=kind-$cluster
devstate=$root/deploy/k8s/dev/.state
image=multica-sandbox-pod:kube-fake
label=app.kubernetes.io/managed-by=sandbox-manager
failed=0
ok() { echo "ok    $*"; }
warn() { echo "warn  $*"; }
bad() { echo "FAIL  $*"; failed=1; }
k() { kubectl --context "$ctx" -n "$ns" "$@"; }

[ "$(id -u)" != 0 ] && ok "non-root user $(id -un)" || bad "running as root: run the Go checks as a non-root user"
for t in go docker kubectl jq curl openssl git; do
	command -v "$t" >/dev/null && ok "$t" || bad "$t is not on PATH"
done
if command -v kind >/dev/null; then ok "kind $(command -v kind)"; else warn "no kind binary: e2e.sh and pod-image.sh use the stand-in $here/kind (no cluster create or delete)"; fi

node=$cluster-control-plane
if [ "$(docker inspect -f '{{.State.Running}}' "$node" 2>/dev/null)" = true ]; then ok "kind node $node running"; else
	bad "kind node $node is not running: deploy/k8s/dev/up.sh (needs the real kind binary to create a cluster)"
	exit 1
fi
kubectl config get-contexts -o name 2>/dev/null | grep -qx "$ctx" && ok "kubectl context $ctx" || bad "no kubectl context $ctx"

for w in statefulset/multica-pg deployment/multica-backend deployment/multica-web; do
	r=$(k get "$w" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
	[ "${r:-0}" -ge 1 ] && ok "$w ready" || bad "$w not ready in $ns: deploy/k8s/dev/up.sh"
done
img=$(k get deployment multica-backend -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
case $img in *:v0.11.0) ok "backend image $img" ;; *) bad "backend image is '$img', want v0.11.0" ;; esac
curl -fsS -m 5 http://127.0.0.1:18080/health >/dev/null && ok "backend answers http://127.0.0.1:18080/health" || bad "no backend at http://127.0.0.1:18080"
k get deployment sandbox-e2e-owner >/dev/null 2>&1 && ok "owner Deployment sandbox-e2e-owner" || warn "no sandbox-e2e-owner: the owner scenario is skipped (kubectl -n $ns apply -f deploy/k8s/dev/owner.yaml)"

for f in backend.env dev-user.kubeconfig manager-db-url payload-key; do
	[ -s "$devstate/$f" ] && ok ".state/$f" || bad "deploy/k8s/dev/.state/$f missing: deploy/k8s/dev/up.sh"
done
if [ -s "$devstate/dev-user.kubeconfig" ]; then
	[ "$(KUBECONFIG=$devstate/dev-user.kubeconfig kubectl auth can-i create pods 2>/dev/null)" = yes ] &&
		ok "dev-user may create pods" || bad "dev-user kubeconfig may not create pods"
	left=$(KUBECONFIG=$devstate/dev-user.kubeconfig kubectl get pods -l "$label" -o name 2>/dev/null)
	[ -z "$left" ] && ok "no managed pods in $ns" || bad "managed pods in $ns (a manager running, or left over; e2e refuses): $(echo $left)"
fi

host_id=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null)
node_id=$(docker exec "$node" crictl inspecti -o go-template --template '{{.status.id}}' "docker.io/library/$image" 2>/dev/null)
if [ -z "$host_id" ]; then bad "no $image on the host: scripts/pod-image.sh"
elif [ "$host_id" != "$node_id" ]; then bad "$image on the node (${node_id:-none}) is not the host's ($host_id): scripts/pod-image.sh"
else ok "$image loaded into $node (${host_id:7:12})"; fi
if [ -n "$host_id" ]; then
	inputs=(deploy/Dockerfile.pod deploy/Dockerfile.pod.dockerignore deploy/pod deploy/fake deploy/home)
	built=$(date -d "$(docker image inspect -f '{{.Created}}' "$image")" +%s)
	changed=$(git -C "$root" log -1 --format=%ct -- "${inputs[@]}")
	dirty=$(git -C "$root" status --porcelain -- "${inputs[@]}")
	if [ -n "$dirty" ]; then warn "uncommitted changes to the pod image's inputs: rebuild with scripts/pod-image.sh"
	elif [ "${changed:-0}" -gt "$built" ]; then warn "the pod image's inputs changed after it was built: scripts/pod-image.sh"
	else ok "$image built after its inputs last changed"; fi
fi
src=${MULTICA_SRC:-$root/../multica}
b=$(git -C "$src" branch --show-current 2>/dev/null)
[ "$b" = security/v0.11.0 ] && ok "Multica source $src on security/v0.11.0" || warn "Multica source $src is on '${b:-?}', want security/v0.11.0 (only pod-image.sh needs it)"

for p in 8791 8792 8793 8796 8797; do
	if ss -ltnH "sport = :$p" 2>/dev/null | grep -q .; then warn "port $p in use (8791 devpods, 8792/8793 manager.sh, 8796/8797 e2e): $(ss -ltnpH "sport = :$p" 2>/dev/null | grep -o 'users:.*' | head -1)"; else ok "port $p free"; fi
done
[ -n "${SANDBOX_TEST_DATABASE_URL:-}" ] && ok "SANDBOX_TEST_DATABASE_URL set" || ok "SANDBOX_TEST_DATABASE_URL unset: go-checks.sh uses .state/manager-db-url"

[ "$failed" = 0 ] && echo "doctor: ready" || echo "doctor: not ready"
exit "$failed"
