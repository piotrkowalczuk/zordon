#!/usr/bin/env bash
# Claim: a federated child (project/) attaches to the parent's Caddy and
# is reachable on the composed domain httpbin.<pathhash>.test, and
# `zordon status` surfaces that composed URL in its print line.
cd "$(dirname "$0")"
source ../_lib.sh
# macOS-only (see dir name + `make e2e` *_macos gate): the parent's
# sudo hooks need pfctl + /etc/resolver, absent on Linux. Defensive
# guard for direct invocation.
[ "$(uname -s)" = Darwin ] || skip "federation_macos: needs macOS pf/resolver ($(uname -s))"
need curl
need_net
need_sudo

build_bins
cd project
trap 'zordon stop --agent >/dev/null 2>&1 || true' EXIT
zordon sudo
info "zordon start (federated child)"
zordon start --agent --timeout 180s

status="$(zordon status --agent 2>&1)" || fail "status failed"
assert_contains "$status" "httpbin" "child service present"
host="$(echo "$status" | sed -nE 's#.*http://(httpbin\.[a-f0-9]+\.test:[0-9]+)/.*#\1#p' | head -1)"
[ -n "$host" ] || fail "composed domain not found in status print: $status"
pass "composed domain in print: $host"
body="$(http_get "http://$host/")" || fail "not reachable via Caddy at $host"
assert_contains "$body" "go-httpbin" "httpbin reachable through federation"
