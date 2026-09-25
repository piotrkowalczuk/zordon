#!/usr/bin/env bash
# Every Go test skip must go through ztest.Skip, which fails instead of
# skipping when ZORDON_NO_SKIP=1 (CI). A direct t.Skip*/b.Skip* would bypass
# that and let a missing tool turn a suite green without running it.
set -euo pipefail
cd "$(dirname "$0")/.."

hits="$(grep -rnE '\.(Skip|Skipf|SkipNow)\(' --include='*.go' \
	--exclude-dir=.zordon --exclude-dir=_site --exclude-dir=workspaces --exclude-dir=.claude \
	. | grep -v '^\./internal/ztest/' | grep -vE 'ztest\.Skip\(' || true)"

if [ -n "$hits" ]; then
	echo "direct test skips found; use ztest.Skip(t, reason) instead:" >&2
	echo "$hits" >&2
	exit 1
fi
