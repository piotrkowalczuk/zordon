#!/usr/bin/env bash
# Claim: redis's `backup` provision locates and runs `pg_dump` — a binary
# that ships with POSTGRES's mise package, not redis's — via
# fs::service::bin(service.pkg.postgres) + env::prepend. If the cross-service
# lookup failed, pg_dump wouldn't be on PATH, backup would fail, and (because
# redis gates on backup.success) `zordon start` itself would fail. The dump
# file is the positive proof.
cd "$(dirname "$0")"
source ../_lib.sh
need_net # postgres + redis build from source on first run

start

dump="$EXROOT/.zordon/worktrees/main/dump.sql"
assert_present "$dump"
[ -s "$dump" ] || fail "dump file is empty — pg_dump produced no output"

# pg_dump emits a recognizable header; finding it proves the postgres binary
# (not some unrelated tool) ran, located purely through fs::service::bin.
if grep -q "PostgreSQL database dump" "$dump"; then
	pass "pg_dump from postgres's toolchain ran via fs::service::bin"
else
	fail "dump present but missing pg_dump header — head: $(head -1 "$dump")"
fi
