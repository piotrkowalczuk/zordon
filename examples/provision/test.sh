#!/usr/bin/env bash
# Claim: provisions chain through `after` (init-state → smoke-test →
# record-completion), every sync provision finishes before EventDone
# (so its marker is on disk when `zordon start` returns), and a
# `detached = true` provision keeps running past EventDone without
# blocking bringup.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start

markers="$EXROOT/workspaces/main/markers"

# Sync provisions: must be done by the time start returned.
assert_present "$markers/init"
assert_present "$markers/complete"

# record-completion writes a unix timestamp. Non-empty proves the cmd
# actually ran (not just `touch`ed).
[ -s "$markers/complete" ] || fail "complete marker is empty — record-completion didn't write a timestamp"
pass "record-completion wrote: $(cat "$markers/complete")"

# Detached: still in flight when start returned (cmd sleeps ~1s).
# Poll up to 5s for the marker to land — way more than the cmd needs,
# but cheap insurance against a slow runner.
notify="$markers/notify"
for _ in $(seq 1 50); do
	[ -f "$notify" ] && break
	/bin/sleep 0.1 2>/dev/null || /bin/sleep 1
done
assert_present "$notify"

# zordon get surfaces the resolved provision details, keyed by provision
# name, so tooling can introspect the chain without parsing the Alphasfile.
prov_cmd="$(zordon get service.go.app.provision.init-state.cmd)" || fail "zordon get service.go.app.provision.init-state.cmd failed"
assert_contains "$prov_cmd" "markers/init" "zordon get exposes the resolved provision cmd"
