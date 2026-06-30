#!/usr/bin/env bash
# Claim: a worktree stranded mid-setup by a failfast kill — its git
# worktree still carries the `initializing` lock and its sparse sources
# never finished checking out — must be REBUILT on the next
# `zordon start`, not silently reused on the bare existence of
# <dest>/.git. Reusing it strands the supervisor with a tree missing its
# workspace sources (the @bnpl-ui/shared / vite failures of issue #38).
EXDIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXDIR"
source ../_lib.sh
need git
need curl

# A named workspace checks out HEAD; if the service source isn't
# committed the sparse path can't materialize — a prerequisite, not a bug.
git -C "$ROOT" cat-file -e "HEAD:examples/worktree_selfheal/src/app/main.go" 2>/dev/null \
	|| skip "examples/worktree_selfheal/src/app not committed (a @HEAD workspace can't see it); commit the source to run this"

build_bins

wt="$EXDIR/workspaces/heal/src/app"                  # per-service worktree root
src="$wt/examples/worktree_selfheal/src/app"         # sparse-checked-out source subtree

# A previously interrupted run can leave this worktree LOCKED, and
# reset_state's `git worktree prune` cannot reclaim a locked tree. Unlock
# and remove it first — exactly the recovery the fix makes routine.
git -C "$ROOT" worktree unlock "$wt" >/dev/null 2>&1 || true
git -C "$ROOT" worktree remove --force "$wt" >/dev/null 2>&1 || true
reset_state heal
zordon workspace create heal
trap 'cd "$EXDIR"; git -C "$ROOT" worktree unlock "$wt" >/dev/null 2>&1 || true; zordon stop --agent >/dev/null 2>&1 || true' EXIT

# Baseline: the freshly created workspace builds and serves.
cd "$EXDIR/workspaces/heal"
info "zordon start (baseline)"
zordon start --agent --timeout 120s
port="$(port_of "/app -addr")" || fail "no port for app (baseline)"
body="$(http_get "http://127.0.0.1:$port/")" || fail "app not responding (baseline)"
assert_contains "$body" "service=app" "app serves before interruption"
zordon stop --agent >/dev/null 2>&1 || true

# Strand the worktree the way a failfast kill does: re-apply the
# `initializing` lock the running binary writes at `worktree add`, and
# drop the sparse source subtree (its checkout never completed).
assert_present "$src/main.go"
git -C "$ROOT" worktree lock --reason initializing "$wt"
rm -rf "$src"
assert_absent "$src"
info "worktree stranded mid-init (locked=initializing, source removed)"

# The next start must heal it: detect the initializing lock, rebuild the
# worktree, re-materialize the source. Under the bug it reuses the broken
# tree (a no-op on the bare .git), the source stays gone, and the go build
# fails — so the start below fails and the source remains absent.
cd "$EXDIR/workspaces/heal"
info "zordon start (after interrupted setup)"
zordon start --agent --timeout 120s || true
[ -e "$src/main.go" ] \
	|| fail "interrupted worktree reused, not rebuilt: $src/main.go still missing (issue #38)"
pass "interrupted worktree rebuilt: source re-materialized"

port="$(port_of "/app -addr")" || fail "no port for app after recovery"
body="$(http_get "http://127.0.0.1:$port/")" || fail "app not responding after recovery"
assert_contains "$body" "service=app" "app recovered after interrupted setup"
