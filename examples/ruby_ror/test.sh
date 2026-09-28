#!/usr/bin/env bash
# Claim: a Rails app comes up under zordon end to end — `bundle install`
# with the declared bundler from the mise-pinned ruby, the `db` provision
# runs `bin/rails db:prepare` against a SQLite file in fs::var() before
# puma starts, readiness passes on Rails' /up health check, and GET /
# writes through ActiveRecord (the visit counter grows). Nothing lands in
# the source tree: no vendor/bundle, .bundle/config, db/schema.rb, log/,
# tmp/ or database file.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need_net

start
port="$(zordon get service.ruby.web.vars.port)" || fail "could not read web port"
db="$(zordon get service.ruby.web.env.DATABASE_PATH)" || fail "could not read DATABASE_PATH"

up="$(http_get "http://127.0.0.1:$port/up")" || fail "/up not responding"
first="$(http_get "http://127.0.0.1:$port/")" || fail "GET / not responding"
second="$(http_get "http://127.0.0.1:$port/")" || fail "GET / not responding"
assert_contains "$first" '"visits":1' "first visit recorded"
assert_contains "$second" '"visits":2' "second visit recorded"
assert_contains "$first" '"rails":"8.1.' "rails 8.1 booted"
assert_contains "$first" '"bundler":"2.5.6"' "declared bundler"
assert_present "$db"
for leftover in vendor/bundle .bundle/config db/schema.rb log tmp development.sqlite3; do
	assert_absent "src/app/$leftover"
done
