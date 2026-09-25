#!/usr/bin/env bash
# Go tests never skip. A missing prerequisite is a broken environment and the
# test must fail saying what is missing; a skip exits green and `go test`
# without -v does not even print it.
set -euo pipefail
cd "$(dirname "$0")/.."

hits="$(grep -rnE '\.(Skip|Skipf|SkipNow)\(' --include='*.go' \
	--exclude-dir=.zordon --exclude-dir=_site --exclude-dir=workspaces --exclude-dir=.claude \
	. || true)"

if [ -n "$hits" ]; then
	echo "Go tests must not skip; fail with t.Fatal and name the missing prerequisite:" >&2
	echo "$hits" >&2
	exit 1
fi
