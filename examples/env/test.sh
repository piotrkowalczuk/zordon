#!/usr/bin/env bash
# Claim: env precedence — process < dotenv < env{}. The env{} block's
# OVERRIDE_ME must win over the dotenv's; both static and dynamic
# (port-interpolated) values are injected.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
env="$(http_get "http://127.0.0.1:$port/env")" || fail "/env not responding"

# /env dumps the whole os.Environ(); the process legitimately inherits
# the parent env, so we don't assert "only these". We DO assert that
# every Alphasfile-managed key resolves to EXACTLY the expected line,
# appears EXACTLY once, and that the overridden dotenv value is gone.
assert_line() { # <key=value>
	grep -Fxq -- "$1" <<<"$env" || fail "expected exact env line '$1'"
	local k="${1%%=*}" n
	n="$(grep -c "^${k}=" <<<"$env")"
	[ "$n" = 1 ] || fail "env key $k appears $n times, want exactly 1"
	pass "exact, unique: $1"
}
assert_line "ENV_STATIC=hello"
assert_line "DOTENV_FROM_FILE=1"
assert_line "DOTENV2_FROM_FILE=1"          # second dotenv file in the list
assert_line "ENV_DYN=127.0.0.1:$port"
assert_line "OVERRIDE_ME=from-env"         # env{} wins over dotenv
assert_line "DOTENV_ORDER=second"          # later dotenv file wins over earlier
grep -Fxq -- "OVERRIDE_ME=from-dotenv" <<<"$env" \
	&& fail "overridden dotenv value leaked (OVERRIDE_ME=from-dotenv present)" \
	|| pass "overridden dotenv value absent"
grep -Fxq -- "DOTENV_ORDER=first" <<<"$env" \
	&& fail "earlier dotenv value leaked (DOTENV_ORDER=first present)" \
	|| pass "earlier dotenv value overridden by later file"
