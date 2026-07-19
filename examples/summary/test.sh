#!/usr/bin/env bash
# Claim: `zordon start --summary` prints, after a successful bringup, a
# "Bringup complete" timing report — each service's phases (wait / build /
# spawn / ready / total) in start order plus each provision's run. `api`
# waits on `db`, so it starts second and its wait phase is non-zero.
#
# The summary is CLIENT-side output (zordon's own stderr), so it lands in
# $ZORDON_LOG (captured by start), not the alpha log — assert against it
# with assert_zordon_contains.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start --summary

assert_zordon_contains "Bringup complete" "summary block printed"
assert_zordon_contains "db" "db service listed"
assert_zordon_contains "api" "api service listed"
assert_zordon_contains "wait" "phase columns present"
assert_zordon_contains "total" "phase columns present"
assert_zordon_contains "toolchain.go@ready" "implicit toolchain dep surfaced"
assert_zordon_contains "service.go.db.runtime@ready" "api dependency shown"
assert_zordon_contains "long pole" "bottleneck dep flagged"
assert_zordon_contains "provisions:" "provisions section present"
assert_zordon_contains "migrate" "migrate provision listed"
