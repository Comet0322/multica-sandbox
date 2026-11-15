#!/usr/bin/env bash
# Delete the dev kind cluster and the state up.sh kept.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
kind delete cluster --name "${CLUSTER:-sandbox-dev}"
rm -rf "${STATE:-$here/.state}"
