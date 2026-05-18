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

# `zordon get`: same resolved value via dotted path and Go template.
got="$(zordon get service.go.app.vars.port)" || fail "get (path) failed"
[ "$got" = "$port" ] && pass "get path: vars.port == $port" \
	|| fail "get path: vars.port=$got, want $port"

tpl="$(zordon get '{{ .service.go.app.vars.port }}')" || fail "get (template) failed"
[ "$tpl" = "$port" ] && pass "get template: vars.port == $port" \
	|| fail "get template: vars.port=$tpl, want $port"

assert_contains "$(zordon get service.go.app.print)" \
	"http://127.0.0.1:$port/" "get: resolved print line"
