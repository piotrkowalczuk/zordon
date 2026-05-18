#!/usr/bin/env bash
# Claim: a Ruby service (no-op build, stdlib server) serves HTTP, and
# its startup `puts "up ..."` line reaches the alpha log — proving the
# PTY default for ruby actually defeats Ruby's pipe-mode block buffering
# without touching the app source.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need ruby

start
port="$(port_of "app.rb -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "ruby-example ok" "ruby service body"
assert_log_contains "up 127.0.0.1:$port" "ruby startup stdout"
