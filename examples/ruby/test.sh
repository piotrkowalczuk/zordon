#!/usr/bin/env bash
# Claim: a Ruby service builds with the default `bundle install` and runs
# under `bundle exec` with the bundler the Alphasfile declares — from the
# mise-pinned ruby, not from the host — its startup `puts "up ..."` line
# reaches the alpha log (the PTY default defeats Ruby's pipe-mode block
# buffering), and the build leaves no vendor/bundle or .bundle/config in
# the source tree. No host ruby is needed: mise installs the pinned one.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "app.rb -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "ruby-example ok" "ruby service body"
assert_contains "$body" "bundler 2.5.6" "declared bundler resolved by bundle exec"
assert_log_contains "up 127.0.0.1:$port" "ruby startup stdout"
assert_absent "src/app/vendor/bundle"
assert_absent "src/app/.bundle/config"
