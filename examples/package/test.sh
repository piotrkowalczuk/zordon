#!/usr/bin/env bash
# Claim: a working place outside the project runs a whole stack from a
# one-line Alphasfile that imports the stack by its Go-style path, and a
# zordon.work search entry serves that path from a local checkout. Features
# switch parts of a package on: with both, Caddy serves Hugo on the host
# hugo.test and resolves through CoreDNS; with hugo alone, Caddy proxies every
# path to Hugo; importing Caddy alone runs Caddy alone. Search-resolved
# repositories are not locked. The coredns resolver feature writes
# /etc/resolver and needs root, so it is covered by the Go oracle test only.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need_net

build_bins

# A place must live outside the repository: under it, walk-up would find
# examples/package/Alphasfile as a federation level.
make_place() { # <Alphasfile body>
	local dir
	dir="$(mktemp -d)"
	printf '%s\n' "$1" >"$dir/Alphasfile"
	printf 'search "%s" {}\n' "$ROOT" >"$dir/zordon.work"
	echo "$dir"
}

stop_place() { (cd "$1" && zordon stop --agent >/dev/null 2>&1) || true; pkill -f "$1/" >/dev/null 2>&1 || true; }

start_place() { # <dir>
	info "zordon start in $1"
	(cd "$1" && zordon start --agent --timeout 900s --alpha-log "$1/alpha.log" 2>&1 | tee "$1/zordon.log")
}

caddy_http() { (cd "$1" && zordon get package.caddy.module.caddy.service.go.caddy.vars.http); }

ALL="$(make_place 'import "github.com/piotrkowalczuk/zordon/examples/package@main" {}')"
HUGO="$(make_place 'import "github.com/piotrkowalczuk/zordon/examples/package/caddy@main" { features = ["hugo"] }')"
CADDY="$(make_place 'import "github.com/piotrkowalczuk/zordon/examples/package/caddy@main" {}')"
trap 'stop_place "$ALL"; stop_place "$HUGO"; stop_place "$CADDY"' EXIT

# --- whole stack, both features ---
start_place "$ALL"
plan="$(cd "$ALL" && zordon --agent plan)" || fail "zordon plan failed in $ALL"
assert_contains "$plan" "# import $ROOT/examples/package as stack (search $ROOT)" "the one-liner resolves through zordon.work"
assert_contains "$plan" "# import $ROOT/examples/package/caddy as caddy [features: coredns, hugo]" "the stack imports caddy with both features"
assert_contains "$plan" "# import $ROOT/examples/package/hugo as hugo" "feature hugo requires the hugo package"
assert_contains "$plan" "# import $ROOT/examples/package/coredns as coredns" "feature coredns requires the coredns package"

status="$(cd "$ALL" && zordon status --agent)"
for svc in caddy/caddy/caddy hugo/hugo/hugo coredns/coredns/coredns; do
	assert_contains "$status" "$svc" "$svc is part of the stack"
done
http="$(caddy_http "$ALL")"
assert_contains "$status" "http://hugo.test:$http/" "caddy prints where hugo is"

page="$(curl -fsS --max-time 5 -H 'Host: hugo.test' "http://127.0.0.1:$http/")" || fail "caddy did not serve the hugo.test host on $http"
assert_contains "$page" "zordon-hugo-ok" "caddy routes the hugo.test host to hugo"
assert_contains "$page" "zordon package example" "hugo renders the title input's default"
page="$(curl -fsS --max-time 5 "http://127.0.0.1:$http/" || true)"
case "$page" in *zordon-hugo-ok*) fail "with DNS, hugo must be routed by host only" ;; esac
pass "other hosts do not reach hugo"
dns="$(http_get "http://127.0.0.1:$http/dns/healthz")" || fail "caddy did not serve /dns/healthz"
[ "$dns" = ok ] || fail "/dns/healthz returned '$dns', want ok"
pass "caddy resolves health.test through coredns"
assert_absent "$ALL/zordon.lock"
stop_place "$ALL"

# --- caddy with hugo, no DNS ---
start_place "$HUGO"
status="$(cd "$HUGO" && zordon status --agent)"
assert_contains "$status" "hugo/hugo/hugo" "feature hugo alone starts hugo"
case "$status" in *coredns/coredns/coredns*) fail "coredns started without its feature:\n$status" ;; esac
http="$(caddy_http "$HUGO")"
page="$(http_get "http://127.0.0.1:$http/")" || fail "caddy did not serve / on $http"
assert_contains "$page" "zordon-hugo-ok" "without DNS, caddy proxies every path to hugo"
stop_place "$HUGO"

# --- caddy alone, features off ---
start_place "$CADDY"
plan="$(cd "$CADDY" && zordon --agent plan)" || fail "zordon plan failed in $CADDY"
assert_contains "$plan" "# import $ROOT/examples/package/caddy as caddy (search $ROOT)" "caddy is importable on its own"
case "$plan" in *"as hugo"* | *"as coredns"*) fail "a switched-off require must not import its package:\n$plan" ;; esac
pass "switched-off requires import nothing"

status="$(cd "$CADDY" && zordon status --agent)"
assert_contains "$status" "caddy/caddy/caddy" "caddy runs"
case "$status" in *hugo/hugo/hugo* | *coredns/coredns/coredns*) fail "hugo or coredns started without their feature:\n$status" ;; esac
pass "hugo and coredns stay out"

http="$(caddy_http "$CADDY")"
[ "$(http_get "http://127.0.0.1:$http/healthz")" = ok ] || fail "caddy /healthz"
page="$(curl -fsS --max-time 3 "http://127.0.0.1:$http/" || true)"
case "$page" in *zordon-hugo-ok*) fail "the hugo route exists without the hugo feature" ;; esac
pass "no hugo route without the feature"
