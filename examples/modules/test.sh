#!/usr/bin/env bash
# Claim: `module "<m>" {}` namespaces services. Two modules each own a `db`
# and an `api` with colliding names; the entrypoint's gateway reaches them
# through module.<m>.service.go.api refs. All five run side by side,
# `zordon get` addresses them under module.*, `zordon plan` renders the
# module blocks, and a pick by display name (`auth/api`) brings up only that
# module's subgraph.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

for svc in gateway payments/db payments/api auth/db auth/api; do
	port="$(port_of "-name $svc")" || fail "no port for $svc"
	body="$(http_get "http://127.0.0.1:$port/")" || fail "$svc not responding"
	assert_contains "$body" "service=$svc" "$svc identifies itself"
done

pay_api="$(zordon get module.payments.service.go.api.vars.port)" || fail "zordon get module.payments.service.go.api.vars.port failed"
gw_port="$(port_of "-name gateway")" || fail "no port for gateway"
body="$(http_get "http://127.0.0.1:$gw_port/")" || fail "gateway not responding"
assert_contains "$body" "payments=127.0.0.1:$pay_api" "gateway wired to payments/api through a module.* ref"

plan="$(zordon --agent plan)" || fail "zordon plan failed"
assert_contains "$plan" 'module "payments" {' "plan renders the payments module block"
assert_contains "$plan" 'module "auth" {' "plan renders the auth module block"

# Picks use display names. auth/api pulls in auth/db through its `after`;
# payments stays down.
zordon stop --agent >/dev/null 2>&1 || true
info "zordon start auth/api"
zordon start --agent --timeout 90s --alpha-log "$ALPHA_LOG" auth/api 2>&1 | tee -a "$ZORDON_LOG"
port_of "-name auth/db" >/dev/null || fail "auth/db must come up as auth/api's dependency"
pass "auth/db came up with the auth/api pick"
if port_of "-name payments/db" >/dev/null; then
	fail "payments/db must not start on an auth/api pick"
fi
pass "payments/db stayed down"
