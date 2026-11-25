#!/usr/bin/env bash
# The Go checks of AGENTS.md: gofmt, go vet, go test -race, with the store's
# database tests run against a real Postgres. Run as a non-root user.
#
#   .claude/skills/verify-sandbox-manager/scripts/go-checks.sh
#
# SANDBOX_TEST_DATABASE_URL names a Postgres the tests may create schemas in;
# unset, the kind namespace's manager database (.state/manager-db-url) is
# used. With neither, the store tests skip and this script fails.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
cd "$root"
[ "$(id -u)" != 0 ] || { echo "run as a non-root user" >&2; exit 1; }
if [ -z "${SANDBOX_TEST_DATABASE_URL:-}" ] && [ -s deploy/k8s/dev/.state/manager-db-url ]; then
	SANDBOX_TEST_DATABASE_URL=$(cat deploy/k8s/dev/.state/manager-db-url)
fi
[ -n "${SANDBOX_TEST_DATABASE_URL:-}" ] || { echo "set SANDBOX_TEST_DATABASE_URL, or run deploy/k8s/dev/up.sh" >&2; exit 1; }
export SANDBOX_TEST_DATABASE_URL

unformatted=$(git ls-files -z '*.go' | xargs -0 gofmt -l)
[ -z "$unformatted" ] || { echo "gofmt: $unformatted" >&2; exit 1; }
echo "gofmt: clean"
go vet ./...
echo "go vet: clean"
go test -race -count=1 ./...
skipped=$(go test -count=1 -v ./internal/store/ 2>&1 | grep -c -- '--- SKIP' || true)
[ "$skipped" = 0 ] || { echo "the store tests skipped $skipped tests: is the database reachable?" >&2; exit 1; }
echo "store tests: ran against $(sed -E 's|//[^@]*@|//|; s|\?.*||' <<<"$SANDBOX_TEST_DATABASE_URL")"
