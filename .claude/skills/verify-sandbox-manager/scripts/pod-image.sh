#!/usr/bin/env bash
# Build the fake-agent pod image (the stock daemon from the Multica fork plus
# deploy/fake/fake-claude) and load it into the kind cluster. Run it after any
# change to the pod image's inputs (doctor.sh says when).
#
#   .claude/skills/verify-sandbox-manager/scripts/pod-image.sh
#
# MULTICA_SRC is the Multica checkout to build the daemon from (default
# ../multica), on the company fork's branch security/v0.11.0; a worktree of it
# works too. CLUSTER is the kind cluster (default sandbox-dev).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
src=${MULTICA_SRC:-$root/../multica}
cluster=${CLUSTER:-sandbox-dev}
image=multica-sandbox-pod:kube-fake
command -v kind >/dev/null || PATH=$here:$PATH
b=$(git -C "$src" branch --show-current)
[ "$b" = security/v0.11.0 ] || { echo "$src is on '$b', want security/v0.11.0 (set MULTICA_SRC)" >&2; exit 1; }
echo "building $image from $src at $(git -C "$src" rev-parse --short HEAD)"
docker build -q -f "$root/deploy/Dockerfile.pod" --build-context multica="$src" --target fake -t "$image" "$root"
kind load docker-image --name "$cluster" "$image"
echo "loaded $image ($(docker image inspect -f '{{.Id}}' "$image" | cut -c8-19)) into $cluster"
