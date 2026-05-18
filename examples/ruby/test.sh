#!/usr/bin/env bash
# Claim: a Ruby service (no-op build, stdlib server) serves HTTP.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need ruby

start
port="$(port_of "app.rb -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "ruby-example ok" "ruby service body"
