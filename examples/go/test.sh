#!/usr/bin/env bash
# Claim: a Go service built from src=../.. comes up and serves HTTP.
# Bonus: the startup `fmt.Printf("up ...")` line lands in the alpha log
# (Go writes through write(2), so this works under a pipe — no PTY).
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "go-example ok" "go service body"
assert_log_contains "up 127.0.0.1:$port" "go startup stdout"
