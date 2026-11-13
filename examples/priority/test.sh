#!/usr/bin/env bash
# Claim:
#   `alpha` unconditionally lowers its POSIX scheduling priority to nice >= 10
#   at startup, and every process in its tree (builds, provisions, runtime
#   services) inherits that lowered priority without any CLI flag.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need git
need go

start

alpha_nice="$(sed -nE 's/.*starting pid=[0-9]+ socket=[^ ]+ nice=([0-9]+).*/\1/p' "$ALPHA_LOG" | tail -1)"
if [ -z "$alpha_nice" ] || [ "$alpha_nice" -lt 10 ]; then
	fail "expected alpha nice >= 10 in alpha.log, got: ${alpha_nice:-<empty>}"
fi
pass "alpha started with lowered priority (nice=$alpha_nice)"

build_nice="$(sed -n 's/^NICE=//p' "$EXROOT/workspaces/main/build-nice.env")"
if [ -z "$build_nice" ] || [ "$build_nice" -lt 10 ]; then
	fail "expected build nice >= 10, got: ${build_nice:-<empty>}"
fi
pass "build subprocess inherited nice=$build_nice"

prov_nice="$(sed -n 's/^NICE=//p' "$EXROOT/workspaces/main/provision-nice.env")"
if [ -z "$prov_nice" ] || [ "$prov_nice" -lt 10 ]; then
	fail "expected provision nice >= 10, got: ${prov_nice:-<empty>}"
fi
pass "provision subprocess inherited nice=$prov_nice"

port="$(port_of bin/app)" || fail "app port not found"
runtime_body="$(http_get "http://127.0.0.1:$port/")"
runtime_nice="$(printf '%s\n' "$runtime_body" | sed -n 's/^nice=//p')"
if [ -z "$runtime_nice" ] || [ "$runtime_nice" -lt 10 ]; then
	fail "expected runtime service nice >= 10, got: ${runtime_body:-<empty>}"
fi
pass "runtime service inherited nice=$runtime_nice"
