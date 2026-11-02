#!/usr/bin/env bash
# Claim: an entrypoint composes remote modules from zordon-modules and local
# packages across a deep dependency tree with `packages = { ... }`:
#   1) Per-subpath `require` blocks in zordon.mod pin
#      `github.com/zordon-io/zordon-modules/coredns` (coredns/v0.1.0) and
#      `github.com/zordon-io/zordon-modules/caddy` (caddy/v0.1.0) independently.
#   2) `coredns = package.coredns` on the caddy import binds Caddy's internal
#      `../coredns` dependency to the top-level coredns instance, merging
#      `health.test` with `orders.test` and `billing.test`.
#   3) `caddy = package.caddy` on `./checkout` shares one Caddy instance across
#      nested `orders` and `billing` packages, merging their `sites` maps.
#   4) `"orders/store" = package.primary_store` binds orders' store dependency
#      to an aliased top-level instance, overriding its scalar label input while
#      merging its map seed entries.
#   5) `"billing/store" = { features = [...], inputs = { ... } }` isolates
#      billing's store under the `billing/store` namespace and injects features
#      and inputs from the entrypoint.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need_net

if [ -d "${ZORDON_MODULES_DIR:-/tmp/zordon-modules}/.git" ]; then
	export GIT_CONFIG_COUNT=1
	export GIT_CONFIG_KEY_0="url.file://${ZORDON_MODULES_DIR:-/tmp/zordon-modules}.insteadOf"
	export GIT_CONFIG_VALUE_0="https://github.com/zordon-io/zordon-modules.git"
fi

start --timeout 600s

plan="$(zordon --agent plan)" || fail "zordon plan failed"
assert_contains "$plan" "as coredns (imported by package caddy) (github.com/zordon-io/zordon-modules@" "top-level coredns bound to caddy's internal coredns import"
assert_contains "$plan" "as caddy [features: dns] (imported by package billing, package orders) (github.com/zordon-io/zordon-modules@" "top-level caddy shared by orders and billing"
assert_contains "$plan" "# import $EXROOT/store as primary_store (imported by package orders)" "top-level aliased store instance bound to orders/store"
assert_contains "$plan" "# import $EXROOT/store as billing/store [features: audit] (imported by package billing)" "isolated billing/store instance with injected audit feature"

status="$(zordon status --agent)"
for svc in coredns/coredns caddy/caddy primary_store/store billing/store/store orders/orders billing/billing; do
	assert_contains "$status" "$svc" "$svc is part of the stack"
done

http="$(zordon get package.caddy.component.caddy.service.go.caddy.vars.http)" || fail "get caddy http port failed"

orders_resp="$(curl -fsS --max-time 5 -H "Host: orders.test" "http://127.0.0.1:$http/orders/")" || fail "caddy did not route orders.test"
assert_contains "$orders_resp" "orders ok (store=primary audit=false keys=orders_schema=v1,shared=true)" "orders uses bound primary_store with merged seed map"

billing_resp="$(curl -fsS --max-time 5 -H "Host: billing.test" "http://127.0.0.1:$http/billing/")" || fail "caddy did not route billing.test"
assert_contains "$billing_resp" "billing ok (store=billing-isolated audit=true keys=billing_schema=v1,tier=enterprise)" "billing uses isolated billing/store with injected feature and inputs"

dns_resp="$(http_get "http://127.0.0.1:$http/dns/healthz")" || fail "caddy did not serve /dns/healthz"
[ "$dns_resp" = ok ] || fail "/dns/healthz returned '$dns_resp', want ok"
pass "caddy resolves health.test through the bound coredns instance"
