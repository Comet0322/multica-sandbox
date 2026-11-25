#!/usr/bin/env bash
# Rollout metrics from Multica's database, read only: one CSV per question in
# an output directory. See plan.md next to this file for what each answers.
#
#   metrics.sh SINCE UNTIL [OUT]
#
#   SINCE, UNTIL  the window, any timestamp Postgres reads ('2026-12-01',
#                 '2026-12-01 09:00+08'); tasks created in [SINCE, UNTIL)
#   OUT           output directory (default ./rollout-metrics-<SINCE>-<UNTIL>)
#
# Environment:
#   MULTICA_DATABASE_URL  a read-only role on Multica's database (required)
#   MANAGER_DAEMON_ID     the manager's SANDBOX_MANAGER_DAEMON_ID; tasks on
#                         runtimes with that ID (or ID-clean) form the managed
#                         arm. Unset before any department moved.
#   FIXED_DAEMON_IDS      comma list of the daemon IDs of the departments'
#                         fixed runtimes (01-runtimes.csv lists every runtime
#                         with its daemon ID). Unset, every runtime not the
#                         manager's counts as fixed, members' laptops included.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
[ $# -ge 2 ] || { sed -n '2,21p' "$0"; exit 2; }
since=$1 until=$2
out=${3:-./rollout-metrics-${since//[^0-9A-Za-z]/}-${until//[^0-9A-Za-z]/}}
: "${MULTICA_DATABASE_URL:?set MULTICA_DATABASE_URL to a read-only role on the Multica database}"
daemon=${MANAGER_DAEMON_ID:-}
mkdir -p "$out"
out=$(cd "$out" && pwd)
psql "$MULTICA_DATABASE_URL" \
	--no-psqlrc --quiet --csv -v ON_ERROR_STOP=1 \
	-v since="$since" -v until="$until" -v daemon="${daemon:-<none>}" -v fixed="${FIXED_DAEMON_IDS:-}" -v out="$out" \
	-f "$here/metrics.sql"
echo "wrote $(ls "$out" | wc -l) files to $out"
