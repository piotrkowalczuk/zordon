#!/usr/bin/env bash
# Claim: a working place outside the project runs a whole stack from an
# Alphasfile of two lines, a require and an import of the stack by its
# Go-style path, and a zordon.work search entry serves that path from a local
# checkout. Two sites, shop and blog, each add an entry to the sites input
# of Caddy, which routes them by host; Caddy's dns feature imports CoreDNS and
# resolves through it. Importing the shop alone brings the Caddy it
# registers with and no blog; importing Caddy alone runs Caddy without
# sites. Search-resolved repositories are not locked. The coredns resolver
# feature writes /etc/resolver and needs root, so it is covered by the Go
# oracle test only.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need_net

build_bins

# A place must live outside the repository: under it, walk-up would find
# examples/package/Alphasfile as a federation level, and the repository's
# zordon.mod would own the require.
make_place() { # <import line>
	local dir
	dir="$(mktemp -d)"
	printf 'require "github.com/piotrkowalczuk/zordon" { ref = "main" }\n%s\n' "$1" >"$dir/Alphasfile"
	printf 'search "%s" {}\n' "$ROOT" >"$dir/zordon.work"
	echo "$dir"
}

stop_place() { (cd "$1" && zordon stop --agent >/dev/null 2>&1) || true; pkill -f "$1/" >/dev/null 2>&1 || true; }

start_place() { # <dir>
	info "zordon start in $1"
	(cd "$1" && zordon start --agent --timeout 900s --alpha-log "$1/alpha.log" 2>&1 | tee "$1/zordon.log")
}

caddy_http() { (cd "$1" && zordon get package.caddy.module.caddy.service.go.caddy.vars.http); }

site() { # <http port> <host>
	curl -fsS --max-time 5 -H "Host: $2" "http://127.0.0.1:$1/"
}

ALL="$(make_place 'import "github.com/piotrkowalczuk/zordon/examples/package" {}')"
SHOP="$(make_place 'import "github.com/piotrkowalczuk/zordon/examples/package/shop" {}')"
CADDY="$(make_place 'import "github.com/piotrkowalczuk/zordon/examples/package/caddy" {}')"
trap 'stop_place "$ALL"; stop_place "$SHOP"; stop_place "$CADDY"' EXIT

# --- whole stack: shop and blog behind caddy, dns on ---
start_place "$ALL"
plan="$(cd "$ALL" && zordon --agent plan)" || fail "zordon plan failed in $ALL"
assert_contains "$plan" "# import $ROOT/examples/package as stack (search $ROOT)" "the import resolves through zordon.work"
assert_contains "$plan" "# import $ROOT/examples/package/caddy as caddy [features: dns] (imported by package blog, package shop, package stack)" "shop, blog and the stack import caddy; the stack turns on dns"
assert_contains "$plan" "# import $ROOT/examples/package/coredns as coredns (imported by package caddy)" "feature dns imports coredns"

status="$(cd "$ALL" && zordon status --agent)"
for svc in caddy/caddy shop/hugo blog/hugo coredns/coredns; do
	assert_contains "$status" "$svc" "$svc is part of the stack"
done
http="$(caddy_http "$ALL")"
assert_contains "$status" "http://shop.test:$http/" "caddy prints the shop's address"
assert_contains "$status" "http://blog.test:$http/" "caddy prints the blog's address"

page="$(site "$http" shop.test)" || fail "caddy did not serve the shop.test host on $http"
assert_contains "$page" "zordon-shop-ok" "caddy routes shop.test to the shop"
assert_contains "$page" "zordon shop" "the shop renders its title input's default"
page="$(site "$http" blog.test)" || fail "caddy did not serve the blog.test host on $http"
assert_contains "$page" "zordon-blog-ok" "caddy routes blog.test to the blog"
page="$(curl -fsS --max-time 5 "http://127.0.0.1:$http/" || true)"
case "$page" in *zordon-shop-ok* | *zordon-blog-ok*) fail "a site answered a host it did not provide" ;; esac
pass "other hosts reach no site"
dns="$(http_get "http://127.0.0.1:$http/dns/healthz")" || fail "caddy did not serve /dns/healthz"
[ "$dns" = ok ] || fail "/dns/healthz returned '$dns', want ok"
pass "caddy resolves health.test through coredns"
assert_absent "$ALL/zordon.lock"
stop_place "$ALL"

# --- the shop alone: it brings caddy, not the blog ---
start_place "$SHOP"
status="$(cd "$SHOP" && zordon status --agent)"
assert_contains "$status" "caddy/caddy" "the shop brings the caddy it registers with"
case "$status" in *blog/hugo* | *coredns/coredns*) fail "the blog or coredns started with the shop alone:\n$status" ;; esac
pass "no blog and no coredns"
http="$(caddy_http "$SHOP")"
page="$(site "$http" shop.test)" || fail "caddy did not serve shop.test on $http"
assert_contains "$page" "zordon-shop-ok" "caddy routes shop.test to the shop"
page="$(site "$http" blog.test || true)"
case "$page" in *zordon-blog-ok*) fail "blog.test is routed without the blog" ;; esac
pass "blog.test is not routed"
stop_place "$SHOP"

# --- caddy alone: no sites, features off ---
start_place "$CADDY"
plan="$(cd "$CADDY" && zordon --agent plan)" || fail "zordon plan failed in $CADDY"
assert_contains "$plan" "# import $ROOT/examples/package/caddy as caddy (search $ROOT)" "caddy is importable on its own"
case "$plan" in *"as coredns"*) fail "a switched-off import must not pull in its package:\n$plan" ;; esac
pass "switched-off imports pull in nothing"

status="$(cd "$CADDY" && zordon status --agent)"
assert_contains "$status" "caddy/caddy" "caddy runs"
case "$status" in *hugo* | *coredns/coredns*) fail "a site or coredns started with caddy alone:\n$status" ;; esac
pass "no site and no coredns"

http="$(caddy_http "$CADDY")"
[ "$(http_get "http://127.0.0.1:$http/healthz")" = ok ] || fail "caddy /healthz"
