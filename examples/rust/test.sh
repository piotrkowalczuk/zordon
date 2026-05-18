#!/usr/bin/env bash
# Claim: a Rust service (cargo install --path) comes up and serves HTTP.
# Bonus: the startup `println!("up ...")` line lands in the alpha log
# (Rust's stdout wraps a LineWriter, flushes on '\n' under any fd — no
# PTY needed).
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need cargo

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "rust-example ok" "rust service body"
assert_log_contains "up 127.0.0.1:$port" "rust startup stdout"
