#!/usr/bin/env bash
# Claim: zordon start, plan and workspace create pick a service by its own
# name when that name is unique in the stack, a whole component, a whole
# package, or one component of a package. A name that selects more than one
# thing, like worker in both shop and billing, is refused with every
# candidate listed.
EXDIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXDIR"
source ../_lib.sh
need curl

build_bins

plan() { zordon --agent plan "$@"; }

out="$(plan grpc)" || fail "zordon plan grpc failed"
assert_contains "$out" 'service "go" "grpc"' "a unique service name picks the service inside its component"
case "$out" in *'service "go" "http"'* | *'service "go" "web"'*) fail "grpc picked more than grpc:\n$out" ;; esac

out="$(plan api)" || fail "zordon plan api failed"
assert_contains "$out" 'service "go" "http"' "a component picks its first service"
assert_contains "$out" 'service "go" "grpc"' "a component picks its second service"
case "$out" in *'service "go" "web"'* | *'package "'*) fail "the component picked more than itself:\n$out" ;; esac

out="$(plan shop)" || fail "zordon plan shop failed"
assert_contains "$out" 'package "shop"' "a package picks its services"
case "$out" in *'package "billing"'* | *'service "go" "web"'*) fail "the package picked more than itself:\n$out" ;; esac

out="$(plan billing/billing)" || fail "zordon plan billing/billing failed"
assert_contains "$out" 'package "billing"' "<package>/<component> picks that component"
case "$out" in *'package "shop"'*) fail "billing/billing picked shop:\n$out" ;; esac

if out="$(plan worker 2>&1)"; then
	fail "worker names a service in shop and in billing, yet plan accepted it:\n$out"
fi
assert_contains "$out" '"worker" names more than one thing' "an ambiguous pick is refused"
assert_contains "$out" "shop/shop/worker" "the refusal lists shop's worker"
assert_contains "$out" "billing/billing/worker" "the refusal lists billing's worker"

trap 'cd "$EXDIR" && zordon stop --agent >/dev/null 2>&1 || true; zordon workspace rm picks >/dev/null 2>&1 || true; reap' EXIT
reset_state main
mkdir -p "$(dirname "$ALPHA_LOG")"
info "zordon start shop"
zordon start --agent --timeout 120s --alpha-log "$ALPHA_LOG" shop 2>&1 | tee "$ZORDON_LOG"
status="$(zordon status --agent)"
assert_contains "$status" "shop/worker — running" "zordon start shop runs the shop"
case "$status" in *"billing/worker — running"* | *"web — running"* | *"api/http — running"*) fail "zordon start shop ran more than the shop:\n$status" ;; esac
pass "nothing outside the shop runs"
zordon stop --agent >/dev/null 2>&1 || true

# A workspace checks out HEAD, so the sources must be committed.
git -C "$ROOT" cat-file -e "HEAD:examples/pick_groups/src/app/main.go" 2>/dev/null \
	|| skip "examples/pick_groups/src/app not committed (a workspace checks out HEAD); commit the sources to run this"
reset_state picks
zordon workspace create picks api
assert_present "$EXDIR/workspaces/picks/src/api/http/.git"
assert_present "$EXDIR/workspaces/picks/src/api/grpc/.git"
assert_absent "$EXDIR/workspaces/picks/src/web"
assert_absent "$EXDIR/workspaces/picks/src/shop"
if out="$(zordon workspace service add --workspace=picks --services=shop@main 2>&1)"; then
	fail "a revision on a package must be refused:\n$out"
fi
assert_contains "$out" "a revision names one service's checkout" "a revision on a package is refused"
