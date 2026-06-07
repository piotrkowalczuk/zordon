#!/usr/bin/env bash
# Claim: `zordon start <service>` brings up just the named service and
# the transitive closure of its runtime.after deps — anything else
# stays down.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

# Re-implement start() so we can pass picks. Same trap + reset_state +
# alpha-log handling as the shared helper.
build_bins
trap 'zordon stop --agent >/dev/null 2>&1 || true; reap' EXIT
reset_state main
mkdir -p "$(dirname "$ALPHA_LOG")"
info "zordon start web   (expect: api pulled in by after, worker stays down)"
zordon start --agent --timeout 90s --alpha-log "$ALPHA_LOG" web

status="$(zordon status --agent 2>&1)" || fail "status failed"
assert_contains "$status" "web — running"  "web running"
assert_contains "$status" "api — running"  "api pulled in via after"
case "$status" in
	*"worker — running"*) fail "worker should not be running (it wasn't picked)";;
	*) pass "worker omitted";;
esac

# Restart with no picks → everything comes up.
info "zordon start             (no picks: bring the rest up)"
zordon start --agent --timeout 90s --alpha-log "$ALPHA_LOG"
status="$(zordon status --agent 2>&1)" || fail "status failed"
assert_contains "$status" "worker — running" "worker now running"

# Unknown pick is a clean error, no alpha contact needed.
info "zordon start nope        (expect: error listing available services)"
if out="$(zordon start --agent --alpha-log "$ALPHA_LOG" nope 2>&1)"; then
	fail "expected non-zero exit for unknown service"
fi
assert_contains "$out" "unknown service" "unknown-pick error"
assert_contains "$out" "api" "error lists available services"

# `zordon plan <service>` subsets statically the same way start does:
# a preflight render of web + its after-dep api, with worker omitted —
# no alpha needed.
info "zordon plan web          (expect: web + api rendered, worker omitted)"
plan="$(zordon plan web 2>&1)" || fail "plan failed"
assert_contains "$plan" 'service "go" "web"' "plan renders picked web"
assert_contains "$plan" 'service "go" "api"' "plan pulls in api via after"
case "$plan" in
	*'service "go" "worker"'*) fail "worker must not appear in a subset plan";;
	*) pass "worker omitted from subset plan";;
esac
