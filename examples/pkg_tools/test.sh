#!/usr/bin/env bash
# Claim: `atlas` — a standalone CLI installed from a mise package backend via
# `toolchain { pkg { tools } }` — is found on PATH through
# fs::toolchain::bin(toolchain.pkg) and runs the `migrate` provision that
# applies a schema to a local SQLite DB. If the tool install or PATH exposure
# failed, atlas wouldn't be found (exit 127), migrate would fail, and — because
# the app gates on migrate.success — `zordon start` itself would fail. The
# migrated SQLite file is the positive proof.
cd "$(dirname "$0")"
source ../_lib.sh
need_net # atlas is downloaded via mise/aqua on first run

start

db="$EXROOT/workspaces/main/app.db"
assert_present "$db"
[ -s "$db" ] || fail "sqlite db is empty — atlas migrate produced nothing"

# atlas applied the migration → the CREATE TABLE text (the `users` table name)
# is stored verbatim in the SQLite file. A driver-free proof the tool ran.
if grep -qa "users" "$db"; then
	pass "atlas migrate apply created the users table (tool found via fs::toolchain::bin(toolchain.pkg))"
else
	fail "db present but 'users' table not found — atlas migration didn't apply"
fi
