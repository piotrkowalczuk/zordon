#!/usr/bin/env bash
# Claim: monorepo (serviceA/B/C share one primary) over a named
# worktree — each service gets its OWN sparse per-service checkout, so
# serviceA's tree must NOT contain serviceB/serviceC sources, yet all
# three still build and serve HTTP without colliding.
EXDIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXDIR"
source ../_lib.sh
need curl

# A named worktree checks out HEAD. If the service sources aren't
# committed yet (e.g. a fresh rename still ?? in git status), the
# sparse paths can't materialize — that's a prerequisite, not a bug.
for s in serviceA serviceB serviceC; do
	git -C "$ROOT" cat-file -e "HEAD:examples/worktree/src/$s/main.go" 2>/dev/null \
		|| skip "examples/worktree/src/$s not committed (a @HEAD worktree can't see it); commit the sources to run this"
done

build_bins
reset_state feature
zordon worktree create feature
trap 'cd "$EXDIR" && zordon stop --agent >/dev/null 2>&1 || true' EXIT

cd "$EXDIR/.zordon/worktrees/feature"
info "zordon start (worktree feature)"
zordon start --agent --timeout 120s

for svc in serviceA serviceB serviceC; do
	port="$(port_of "/$svc -addr")" || fail "no port for $svc"
	body="$(http_get "http://127.0.0.1:$port/")" || fail "$svc not responding"
	assert_contains "$body" "service=$svc" "$svc identifies itself"
done

# Sparse: serviceA's per-service checkout carries only its own subtree.
co="$EXDIR/.zordon/worktrees/feature/src/serviceA"
assert_present "$co/examples/worktree/src/serviceA/main.go"
assert_absent  "$co/examples/worktree/src/serviceB"
assert_absent  "$co/examples/worktree/src/serviceC"
