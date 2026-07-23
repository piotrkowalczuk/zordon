#!/usr/bin/env bash
# Claim: zordon's invocation gate runs `zordon start` ONLY from the project
# root or a workspace dir. A run from a plain subdir — or from inside a service
# checkout that carries its own Alphasfile — is REFUSED with a clear error and
# materializes NO nested `workspaces/` stack. This is the root cause of issue
# #73: without the gate, such a run roots a whole parallel stack whose
# `zordon/main/<svc>` branches later collide with the real stack.
EXDIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXDIR"
source ../_lib.sh
need git
need curl

build_bins
reset_state main

# ── 1. A plain subdir is refused, and leaves no nested stack ──────────────────
deep="$EXDIR/src/app"
rm -rf "$deep/workspaces"
info "zordon start from a plain subdir ($deep)"
out="$(cd "$deep" && zordon start --agent --timeout 30s 2>&1 || true)"
assert_contains "$out" "not a zordon invocation dir" "plain subdir refused"
assert_absent "$deep/workspaces"

# ── 2. A buried Alphasfile inside a managed checkout is refused ───────────────
# Fabricate the exact shape `zordon workspace create` + a service repo that
# carries an Alphasfile would leave: workspaces/<ws>/src/<svc>/Alphasfile. No
# real worktree needed — the gate keys off the path shape plus the OUTER
# project's Alphasfile.
buried="$EXDIR/workspaces/wsX/src/app"
mkdir -p "$buried"
cp "$EXDIR/Alphasfile" "$buried/Alphasfile"
info "zordon start from inside a service checkout ($buried)"
out="$(cd "$buried" && zordon start --agent --timeout 30s 2>&1 || true)"
assert_contains "$out" "managed checkout cannot be a project root" "buried checkout refused"
assert_absent "$buried/workspaces"
rm -rf "$EXDIR/workspaces/wsX"

# ── 3. The real project root still starts and serves ─────────────────────────
trap 'cd "$EXDIR"; zordon stop --agent >/dev/null 2>&1 || true; reap' EXIT
info "zordon start from the project root"
zordon start --agent --timeout 90s
port="$(port_of "/app -addr")" || fail "no port for app"
body="$(http_get "http://127.0.0.1:$port/")" || fail "app not responding"
assert_contains "$body" "service=app" "app serves from the real root"
pass "gate refuses nested roots; the real stack still starts"
