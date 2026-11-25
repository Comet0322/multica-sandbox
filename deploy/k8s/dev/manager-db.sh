#!/usr/bin/env bash
# The manager's database in the dev namespace (up.sh runs it; it may be run
# again on its own): a role and a database sandbox_manager on the dev
# Postgres, a node port the host reaches it at, and in .state:
#   manager-db-url  the URL the manager uses (SANDBOX_DATABASE_URL_FILE)
#   payload-key     the key task payloads are encrypted with
#                   (SANDBOX_PAYLOAD_KEY_FILE)
# The role may create databases: the end-to-end scenarios make one per run.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
cluster=${CLUSTER:-sandbox-dev}
ns=${NAMESPACE:-multica-sandbox}
state=${STATE:-$here/.state}
ctx=kind-$cluster
k() { kubectl --context "$ctx" -n "$ns" "$@"; }
psql() { k exec -i multica-pg-0 -- psql -U multica -d multica -v ON_ERROR_STOP=1 -qtA "$@"; }

mkdir -p "$state" && chmod 700 "$state"
umask 077
[ -s "$state/manager-db-password" ] || openssl rand -hex 16 | tr -d '\n' >"$state/manager-db-password"
[ -s "$state/payload-key" ] || openssl rand -base64 32 >"$state/payload-key"
pw=$(cat "$state/manager-db-password")

psql <<SQL
SELECT format('CREATE ROLE sandbox_manager LOGIN CREATEDB PASSWORD %L', '$pw')
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sandbox_manager') \gexec
ALTER ROLE sandbox_manager LOGIN CREATEDB PASSWORD '$pw';
SELECT 'CREATE DATABASE sandbox_manager OWNER sandbox_manager'
  WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'sandbox_manager') \gexec
SQL
k apply -f "$here/manager-db.yaml" >/dev/null

node=$(kubectl --context "$ctx" get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
printf 'postgres://sandbox_manager:%s@%s:30432/sandbox_manager?sslmode=disable' "$pw" "$node" >"$state/manager-db-url"
echo "manager database: sandbox_manager at $node:30432 (URL in $state/manager-db-url)" >&2
