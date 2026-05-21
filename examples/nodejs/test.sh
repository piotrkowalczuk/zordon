#!/usr/bin/env bash
# Claim: a Node service with package.json#scripts.start serves HTTP via
# the inferred `npm start` command (no explicit runtime { cmd }), and
# its startup `up ...` line reaches alpha's log — proving (a) PM
# auto-detection picked npm (no lockfile = npm default), (b) runtime
# cmd inference picked scripts.start over main/bin, and (c) the
# PTY-default for nodejs defeats process.stdout's pipe-mode block
# buffering without touching the app source.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

start
port="$(port_of "app.js")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "nodejs-example ok" "nodejs service body"
assert_log_contains "up 127.0.0.1:$port" "nodejs startup stdout"
