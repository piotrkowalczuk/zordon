#!/usr/bin/env bash
# Claim: `print` is interpolated and surfaced by `zordon status` with
# the resolved port composed into the URL.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "go-example ok" "service body"

status="$(zordon status --agent 2>&1)" || fail "status failed"
assert_contains "$status" "http://127.0.0.1:$port/  (app endpoint)" "status print line"
