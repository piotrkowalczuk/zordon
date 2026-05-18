#!/usr/bin/env bash
# Claim: env is phase-scoped.
#   build{env}   reaches the build command (baked via ldflags) and does
#                NOT leak into the running process.
#   runtime{env} is injected only into the running service.
#   agent{env}   overrides on top — but only when `zordon --agent`.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

build_bins
reset_state main

fetch() { # <expected VERBOSITY> ; returns body via stdout
	port="$(port_of "/app -addr")" || fail "no app port"
	http_get "http://127.0.0.1:$port/" || fail "endpoint not responding"
}

# --- Run 1: no --agent → agent{} overlay must NOT apply ---
trap 'zordon stop --agent >/dev/null 2>&1 || true; reap' EXIT
info "zordon start (no --agent)"
zordon start --timeout 90s
body="$(fetch)"
assert_contains "$body" "builtby=compiled-with-build-env" "build.env reached the compiler (ldflags)"
assert_contains "$body" "RUNTIME_ONLY=runtime-value"      "runtime.env injected at run"
assert_contains "$body" "VERBOSITY=loud"                  "agent overlay NOT applied without --agent"
grep -qx "BUILD_TAG_at_runtime=" <<<"$body" \
	&& pass "build.env did NOT leak into runtime" \
	|| fail "build-only BUILD_TAG leaked into runtime: $body"

zordon stop --agent >/dev/null 2>&1 || true
reap

# --- Run 2: with --agent → agent{} overlay overrides runtime ---
info "zordon start --agent"
zordon start --agent --timeout 90s
body="$(fetch)"
assert_contains "$body" "VERBOSITY=quiet"            "agent.env overrides runtime under --agent"
assert_contains "$body" "RUNTIME_ONLY=runtime-value" "non-overridden runtime.env still present"
assert_contains "$body" "builtby=compiled-with-build-env" "build.env still applied under --agent"
