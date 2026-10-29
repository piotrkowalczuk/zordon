#!/usr/bin/env bash
# Claim: a package's toolchain {} pins the packages it imports that pin none
# of their own. The entrypoint and the app package pin nothing; stack pins go
# and imports app, so the plan renders that pin inside app's component and app
# builds and runs with it.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

plan="$(zordon --agent plan)" || fail "zordon plan failed"
assert_contains "$(tr -s ' \n' ' ' <<<"$plan")" 'component "app" { toolchain { go { version = "1.27.0" } }' "app runs with the toolchain of stack, which imports it"

port="$(zordon get package.app.component.app.service.go.app.vars.port)" || fail "zordon get of the port failed"
http_get "http://127.0.0.1:$port/" >/dev/null || fail "app does not answer on $port"
pass "app built and runs"
assert_log_contains "toolchain go@1.27.0" "alpha installs the go stack pins"
