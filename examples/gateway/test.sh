#!/usr/bin/env bash
# Claim: a package takes static configuration once (inputs, features),
# collects entries many importers provide (slots), and gives values back
# (outputs). orders and billing each register a route with the gateway,
# which knows neither; billing calls orders through the gateway's url
# output; the entrypoint only configures: a greeting for orders and the
# gateway's access_log feature.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

plan="$(zordon --agent plan)" || fail "zordon plan failed"
assert_contains "$plan" "# import $EXROOT/gateway as gateway [features: access_log] (imported by package billing, package orders)" "orders and billing import the gateway; the entrypoint turns on access_log"

status="$(zordon status --agent)"
for svc in gateway/gateway/gateway orders/orders/orders billing/billing/billing; do
	assert_contains "$status" "$svc" "$svc is part of the stack"
done

port="$(zordon get package.gateway.module.gateway.service.go.gateway.vars.port)" || fail "get gateway port failed"
routes="$(zordon get package.gateway.module.gateway.service.go.gateway.file.routes.body)" || fail "get routes failed"
assert_contains "$routes" '"prefix": "/orders/"' "orders provided its route"
assert_contains "$routes" '"prefix": "/billing/"' "billing provided its route"

page="$(http_get "http://127.0.0.1:$port/orders/")" || fail "the gateway did not proxy /orders/"
assert_contains "$page" "orders ok: hi from the entrypoint" "the gateway routes /orders/ to orders, configured by the entrypoint's input"

page="$(http_get "http://127.0.0.1:$port/billing/")" || fail "the gateway did not proxy /billing/"
assert_contains "$page" "billing ok; orders said: orders ok" "billing reaches orders through the gateway's url output"

sleep 1
assert_log_contains "access GET /orders/" "feature access_log logs proxied requests"
