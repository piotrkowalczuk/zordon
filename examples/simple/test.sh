#!/usr/bin/env bash
# Claim: a heterogeneous stack (nats-server, tansu, prometheus, ruby)
# resolved from git/cargo all reach READY under one Alphasfile.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need cargo
need_net

start
status="$(zordon status --agent 2>&1)" || fail "status failed"
for svc in nats-server tansu prometheus ruby-service; do
	assert_contains "$status" "$svc" "$svc present"
done
assert_contains "$status" "prometheus — running" "prometheus running"
assert_contains "$status" "[ready]"               "a readiness probe passed"
