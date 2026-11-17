#!/usr/bin/env bash
# Claim:
#   1. `net::pickport()` allocates from the non-ephemeral pool 20000..31999 and
#      locks `<ZORDON_HOME>/locks/port-<port>.lock` while the service runs.
#   2. `alpha` holds the picked port bound throughout `build` (so no other
#      process can steal it while the service compiles) and closes the socket
#      holders right before `cmd.Start()` so the service binds cleanly.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need git
need go

start

probe_file="$EXROOT/workspaces/main/build-port.env"
assert_present "$probe_file"
probe_env="$(cat "$probe_file")"
assert_contains "$probe_env" "HELD=true" "alpha held picked port during build"

port="$(sed -n 's/^PORT=//p' "$probe_file")"
if [ -z "$port" ] || [ "$port" -lt 20000 ] || [ "$port" -ge 32000 ]; then
	fail "expected picked port in 20000..31999, got: ${port:-<empty>}"
fi
pass "picked port $port is in non-ephemeral range 20000..31999"

lock_file="${ZORDON_HOME:-$HOME/.zordon}/locks/port-$port.lock"
assert_present "$lock_file"
lock_state="$("$EXROOT/workspaces/main/bin/app" -probe-lock "$lock_file")"
assert_contains "$lock_state" "LOCKED=true" "alpha holds flock on port-$port.lock while service is running"
assert_contains "$(http_get "http://127.0.0.1:$port/")" "addr=127.0.0.1:$port" "service bound reserved port and serves HTTP"
