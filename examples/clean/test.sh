#!/usr/bin/env bash
# Claim: a provision's `clean` snippet is teardown that runs only on
# `zordon clean` (stack stopped), never on a plain `zordon stop`. After
# start the setup markers exist; a bare stop leaves them; `zordon clean`
# removes them.
cd "$(dirname "$0")"
source ../_lib.sh

start

state="$EXROOT/.zordon/worktrees/main"
seeded="$state/data/seeded"
registered="$state/registered"

# Both provisions ran at bringup (non-detached ⇒ done before start returned).
assert_present "$seeded"
assert_present "$registered"

# A plain stop must NOT run clean snippets — the markers survive.
info "zordon stop (clean must NOT run here)"
zordon stop --agent
assert_present "$seeded"
assert_present "$registered"

# Wait for the stack to be fully down before cleaning — clean operates on
# a stopped stack and refuses a running one.
for _ in $(seq 1 100); do
	zordon status --agent 2>/dev/null | grep -q "not running" && break
	/bin/sleep 0.1 2>/dev/null || /bin/sleep 1
done

# zordon clean runs each provision's clean snippet (reverse order) against
# the stopped stack.
info "zordon clean"
zordon clean --agent --timeout 90s --alpha-log "$ALPHA_LOG"

assert_absent "$seeded"
assert_absent "$registered"
assert_absent "$state/data"
pass "clean removed every provision marker"
