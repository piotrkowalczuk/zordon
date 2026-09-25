#!/usr/bin/env bash
# Self-test for the example harness: a SKIP must exit 0 locally and fail
# under ZORDON_NO_SKIP=1, so CI can never read a skipped example as green.
# Run by `make e2e.selftest`; not an example itself (no Alphasfile here).
set -uo pipefail
cd "$(dirname "$0")"

run() { ZORDON_NO_SKIP="$1" bash -c 'source ./_lib.sh; need zordon-test-no-such-tool' >/dev/null 2>&1; echo $?; }

rc=0
local_rc="$(run "")"
strict_rc="$(run 1)"
[ "$local_rc" = 0 ] || { echo "FAIL a local skip must exit 0, got $local_rc" >&2; rc=1; }
[ "$strict_rc" != 0 ] || { echo "FAIL a skip under ZORDON_NO_SKIP=1 must fail, got 0" >&2; rc=1; }
[ "$rc" = 0 ] && echo "OK skip exits 0 locally and fails under ZORDON_NO_SKIP=1"
exit "$rc"
