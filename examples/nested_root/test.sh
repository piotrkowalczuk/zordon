#!/usr/bin/env bash
# Claim: zordon resolves its invocation by walking up — like git from a subdir —
# to the nearest `.workspace` boundary and then the project Alphasfile above it.
# So a run from ANY subdir (a plain project subdir, or inside a workspace's
# service checkout that happens to carry its own Alphasfile) attaches to the
# ENCLOSING stack instead of forking a nested `workspaces/…` one. This is the
# root cause of issue #73: without walk-up, a subdir became a shadow project
# root whose `zordon/main/<svc>` branches later collided with the real stack.
EXDIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXDIR"
source ../_lib.sh
need git

build_bins
reset_state main

# invocation hash (fs::hash) zordon resolves for a run from $1, via `plan`.
plan_hash() { ( cd "$1" && zordon --agent plan 2>/dev/null | sed -nE 's/^# === \[([0-9a-f]+)\].*/\1/p' | head -1 ); }

root_h="$(plan_hash "$EXDIR")"
[ -n "$root_h" ] || fail "no invocation hash from the project root"

# ── 1. A plain subdir resolves to the SAME instance as the root (main) ────────
deep="$EXDIR/src/app"
rm -rf "$deep/workspaces"
info "plan from a plain subdir ($deep)"
sub_h="$(plan_hash "$deep")"
[ "$sub_h" = "$root_h" ] || fail "subdir resolved to $sub_h, not the root's $root_h (a nested root!)"
assert_absent "$deep/workspaces"
pass "plain subdir resolves to main ($root_h), no nested stack"

# ── 2. Inside a workspace's checkout, the marker wins over a buried Alphasfile ─
# Fabricate what `zordon workspace create` + a service repo carrying an
# Alphasfile leaves: a marked workspace with a checkout that has its own
# Alphasfile. Resolution must ignore the buried file and run the WORKSPACE.
ws="$EXDIR/workspaces/wsX"
mkdir -p "$ws/src/app"
: > "$ws/.workspace"
cp "$EXDIR/Alphasfile" "$ws/src/app/Alphasfile"

ws_h="$(plan_hash "$ws")"
buried_h="$(plan_hash "$ws/src/app")"
[ -n "$ws_h" ] || fail "no invocation hash from the workspace dir"
[ "$buried_h" = "$ws_h" ] || fail "buried checkout resolved to $buried_h, not workspace wsX's $ws_h (marker not honored)"
[ "$ws_h" != "$root_h" ] || fail "workspace wsX must not share main's instance"
assert_absent "$ws/src/app/workspaces"
pass "buried checkout resolves to workspace wsX ($ws_h), not a nested root"
rm -rf "$ws"

# ── 3. The real project root still starts and serves ─────────────────────────
trap 'cd "$EXDIR"; zordon stop --agent >/dev/null 2>&1 || true; reap' EXIT
info "zordon start from the project root"
zordon start --agent --timeout 90s
port="$(port_of "/app -addr")" || fail "no port for app"
body="$(http_get "http://127.0.0.1:$port/")" || fail "app not responding"
assert_contains "$body" "service=app" "app serves from the real root"
pass "walk-up resolves subdirs to the enclosing stack; the real stack starts"
