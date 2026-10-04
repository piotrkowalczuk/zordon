#!/usr/bin/env bash
# Claim: an Alphasfile is the index of its directory. Every Alphasfile.<name>
# next to it is a part of the same unit, read without an import and bound by
# the same rules: next to a package's Alphasfile it opens the same package.
# The main package platform and its subpackage ingest each keep one
# component in the Alphasfile and one in a part; components of different
# parts see each other, and a package's API is the union of its parts. A
# part with no Alphasfile next to it (tools/Alphasfile.lint) is never read.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

plan="$(zordon --agent plan)" || fail "zordon plan failed"
for svc in api worker collector store; do
	assert_contains "$plan" "service \"go\" \"$svc\"" "the plan has $svc"
done
case "$plan" in *lint*) fail "tools/Alphasfile.lint has no Alphasfile next to it, yet it was read:\n$plan" ;; esac
pass "a part without an Alphasfile is never read"

start
status="$(zordon status --agent)"
for svc in platform/api/api platform/jobs/worker ingest/collector/collector ingest/store/store; do
	assert_contains "$status" "$svc — running" "$svc runs"
done

port() { zordon get "package.$1.component.$2.service.go.$3.vars.port"; }
api="$(port platform api api)"
collector="$(port ingest collector collector)"
store="$(port ingest store store)"

body="$(http_get "http://127.0.0.1:$api/")" || fail "api does not answer"
assert_contains "$body" "upstream=http://127.0.0.1:$collector" "the main package reads the subpackage's output"
body="$(http_get "http://127.0.0.1:$(port platform jobs worker)/")" || fail "worker does not answer"
assert_contains "$body" "upstream=http://127.0.0.1:$api" "a component in Alphasfile.jobs reads a component in the Alphasfile"
body="$(http_get "http://127.0.0.1:$collector/")" || fail "collector does not answer"
assert_contains "$body" "upstream=http://127.0.0.1:$store" "a component in the Alphasfile reads a component in Alphasfile.store"
body="$(http_get "http://127.0.0.1:$store/")" || fail "store does not answer"
assert_contains "$body" "upstream=72h" "an input declared in a part takes the importer's value"
