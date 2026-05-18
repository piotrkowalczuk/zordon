#!/usr/bin/env bash
# Claim: intra/cross-service interpolation, ldflags-in-build, file
# generation and env precedence are all resolved at runtime. The `api`
# service reaches `cache` via service.go.cache.vars.port and renders a
# generated config; env{} (SOURCE=env-block) wins over both dotenvs.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

# api is the only `/app` process launched with -conf; cache has none.
api_line="$(ps -axww -o args= | grep -F "$EXROOT/.zordon" | grep -F -- '/app -addr' | grep -F -- '-conf' | grep -v grep | head -1 || true)"
[ -n "$api_line" ] || fail "could not find api process"
api_port="$(echo "$api_line" | sed -nE 's/.*-addr 127\.0\.0\.1:([0-9]+).*/\1/p')"
cache_port="$(echo "$api_line" | sed -nE 's/.*-cache 127\.0\.0\.1:([0-9]+).*/\1/p')"
[ -n "$api_port" ] && [ -n "$cache_port" ] || fail "could not parse api/cache ports"

body="$(http_get "http://127.0.0.1:$api_port/")" || fail "api not responding"
assert_contains "$body" "cache=127.0.0.1:$cache_port" "cross-service port interp"
assert_contains "$body" "SOURCE=env-block"            "env{} wins precedence"
assert_contains "$body" "label       = api-"          "generated conf rendered"
case "$body" in
	*"tag(ldflags)=unset"*) fail "ldflags tag not injected by build interp";;
	*"tag(ldflags)="*)      pass "ldflags tag injected via build interp";;
	*) fail "no tag line in body";;
esac

cache_body="$(http_get "http://127.0.0.1:$cache_port/")" || fail "cache not responding"
assert_contains "$cache_body" "addr=127.0.0.1:$cache_port" "cache service up"
