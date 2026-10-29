#!/usr/bin/env bash
# Claim: a stack's toolchain can be pinned in the stack package or in the
# entrypoint. stack_go pins go in its own toolchain {}, so app_go, which it
# imports and which pins nothing, runs with that pin: the plan renders it
# inside app_go's component. stack_rust pins nothing, so app_rust runs with
# the rust the entrypoint pins for the whole level: the plan renders no pin
# inside its component. Both build with their pin and answer.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

plan="$(tr -s ' \n' ' ' <<<"$(zordon --agent plan)")" || fail "zordon plan failed"
assert_contains "$plan" 'package "app_go" { component "app" { toolchain { go { version = "1.27.0" } }' "app_go runs with the toolchain of stack_go, which imports it"
assert_contains "$plan" 'package "app_rust" { component "app" { service "rust" "app" {' "app_rust's component carries no pin of its own"
assert_contains "$plan" 'rust { version = "1.83.0" }' "the entrypoint pins rust for the level"

for pkg in app_go app_rust; do
	tc="${pkg#app_}"
	port="$(zordon get "package.$pkg.component.app.service.$tc.app.vars.port")" || fail "zordon get of $pkg's port failed"
	http_get "http://127.0.0.1:$port/" >/dev/null || fail "$pkg does not answer on $port"
	pass "$pkg built and runs"
done
assert_log_contains "toolchain go@1.27.0" "alpha installs the go that stack_go pins"
assert_log_contains "toolchain rust@1.83.0" "alpha installs the rust the entrypoint pins"
