#!/usr/bin/env bash
# Claim:
#   1. ZORDON_BUILD_MAXCPU / --build.maxcpu serializes heavy service builds on
#      one build slot (logging `waiting for build slot`) and hands the full CPU
#      quota to the active build via CARGO_BUILD_JOBS, BUNDLE_JOBS, MAKEFLAGS,
#      CMAKE_BUILD_PARALLEL_LEVEL, and GOMAXPROCS without overriding explicit
#      `build.env` values.
#   2. Negative `--build.maxcpu` values are rejected at CLI validation time.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need git
need go

ZORDON_BUILD_MAXCPU=2 start

assert_log_contains "build.maxcpu=2" "alpha logged resolved build CPU quota"
assert_log_contains "waiting for build slot" "contending build logged waiting phase"

timeline_file="$EXROOT/workspaces/main/timeline.log"
assert_present "$timeline_file"
timeline="$(tr '\n' ' ' <"$timeline_file" | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//')"
case "$timeline" in
	"svc1:enter svc1:exit svc2:enter svc2:exit"|"svc2:enter svc2:exit svc1:enter svc1:exit")
		pass "builds serialized without overlap ($timeline)"
		;;
	*)
		fail "expected non-overlapping build timeline, got: $timeline"
		;;
esac

svc1_env="$(cat "$EXROOT/workspaces/main/svc1-build.env")"
assert_contains "$svc1_env" "CARGO_BUILD_JOBS=2" "svc1 CARGO_BUILD_JOBS"
assert_contains "$svc1_env" "BUNDLE_JOBS=2" "svc1 BUNDLE_JOBS"
assert_contains "$svc1_env" "MAKEFLAGS=-j2" "svc1 MAKEFLAGS"
assert_contains "$svc1_env" "CMAKE_BUILD_PARALLEL_LEVEL=2" "svc1 CMAKE_BUILD_PARALLEL_LEVEL"
assert_contains "$svc1_env" "GOMAXPROCS=2" "svc1 GOMAXPROCS"

svc2_env="$(cat "$EXROOT/workspaces/main/svc2-build.env")"
assert_contains "$svc2_env" "CARGO_BUILD_JOBS=1" "svc2 explicit build.env override preserved"
assert_contains "$svc2_env" "GOMAXPROCS=2" "svc2 GOMAXPROCS"

p1="$(port_of bin/svc1)" || fail "svc1 port not found"
p2="$(port_of bin/svc2)" || fail "svc2 port not found"
assert_contains "$(http_get "http://127.0.0.1:$p1/")" "service=svc1" "svc1 serving HTTP"
assert_contains "$(http_get "http://127.0.0.1:$p2/")" "service=svc2" "svc2 serving HTTP"

info "verifying --build.maxcpu negative value rejection"
if bad_out="$(zordon --build.maxcpu=-1 status --agent 2>&1)"; then
	fail "expected --build.maxcpu=-1 to fail"
fi
assert_contains "$bad_out" "invalid build.maxcpu" "--build.maxcpu rejects negative values"
