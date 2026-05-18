#!/usr/bin/env bash
# Claim: a Go service built from src=../.. comes up and serves HTTP.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "go-example ok" "go service body"
