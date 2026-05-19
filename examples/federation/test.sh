#!/usr/bin/env bash
# Claim: a federated child (project/) attaches to the parent's Caddy and
# is reachable on the composed domain httpbin.<pathhash>.test, and
# `zordon status` surfaces that composed URL in its print line.
cd "$(dirname "$0")"
source ../_lib.sh
# The parent's sudo hooks are macOS-only: caddy/http80 rewrites
# /etc/pf.conf via pfctl, and coredns/resolver writes
# /etc/resolver/test so the OS resolves the composed *.test domain.
# Linux has neither mechanism, so the claim (reachable on
# httpbin.<hash>.test) can't be proven there — a platform
# prerequisite, not a broken claim ⇒ SKIP.
[ "$(uname -s)" = Darwin ] || skip "federation needs macOS pf/resolver integration; not available on $(uname -s)"
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
