#!/usr/bin/env bash
# Claim: a Rust service (cargo install --path) comes up and serves HTTP.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need cargo

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "rust-example ok" "rust service body"
