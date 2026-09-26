#!/usr/bin/env bash
# Claim: a working place outside the project runs a whole stack from a
# one-line Alphasfile that imports the stack by its Go-style path, and a
# zordon.work search entry serves that path from a local checkout. Features
# switch parts of a package on: with both, Caddy proxies to Hugo and resolves
# through CoreDNS; importing Caddy alone runs Caddy alone, without the gated
# routes. Search-resolved repositories are not locked.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need_net

build_bins

# A place must live outside the repository: under it, walk-up would find
# examples/package/Alphasfile as a federation level.
make_place() { # <identity>
	local dir
	dir="$(mktemp -d)"
	printf 'import "%s" {}\n' "$1" >"$dir/Alphasfile"
	printf 'search "%s" {}\n' "$ROOT" >"$dir/zordon.work"
	echo "$dir"
}

stop_place() { (cd "$1" && zordon stop --agent >/dev/null 2>&1) || true; pkill -f "$1/" >/dev/null 2>&1 || true; }

start_place() { # <dir>
	info "zordon start in $1"
	(cd "$1" && zordon start --agent --timeout 900s --alpha-log "$1/alpha.log" 2>&1 | tee "$1/zordon.log")
}

ALL="$(make_place github.com/piotrkowalczuk/zordon/examples/package@main)"
CADDY="$(make_place github.com/piotrkowalczuk/zordon/examples/package/caddy@main)"
trap 'stop_place "$ALL"; stop_place "$CADDY"' EXIT

# --- whole stack, both features ---
start_place "$ALL"
plan="$(cd "$ALL" && zordon --agent plan)" || fail "zordon plan failed in $ALL"
assert_contains "$plan" "# import $ROOT/examples/package as package (search $ROOT)" "the one-liner resolves through zordon.work"
assert_contains "$plan" "# import $ROOT/examples/package/caddy as caddy [features: coredns, hugo]" "the stack imports caddy with both features"
assert_contains "$plan" "# import $ROOT/examples/package/hugo as hugo" "feature hugo requires the hugo package"
assert_contains "$plan" "# import $ROOT/examples/package/coredns as coredns" "feature coredns requires the coredns package"

status="$(cd "$ALL" && zordon status --agent)"
for svc in caddy/caddy hugo/hugo coredns/coredns; do
	assert_contains "$status" "$svc" "$svc is part of the stack"
done

http="$(cd "$ALL" && zordon get module.caddy.service.go.caddy.vars.http)"
page="$(http_get "http://127.0.0.1:$http/")" || fail "caddy did not serve / on $http"
assert_contains "$page" "zordon-hugo-ok" "caddy proxies / to hugo"
assert_contains "$page" "zordon package example" "hugo renders the title input's default"
dns="$(http_get "http://127.0.0.1:$http/dns/healthz")" || fail "caddy did not serve /dns/healthz"
[ "$dns" = ok ] || fail "/dns/healthz returned '$dns', want ok"
pass "caddy resolves health.test through coredns"
assert_absent "$ALL/zordon.lock"
stop_place "$ALL"

# --- caddy alone, features off ---
start_place "$CADDY"
plan="$(cd "$CADDY" && zordon --agent plan)" || fail "zordon plan failed in $CADDY"
assert_contains "$plan" "# import $ROOT/examples/package/caddy as caddy (search $ROOT)" "caddy is importable on its own"
case "$plan" in *"as hugo"* | *"as coredns"*) fail "a switched-off require must not import its package:\n$plan" ;; esac
pass "switched-off requires import nothing"

status="$(cd "$CADDY" && zordon status --agent)"
assert_contains "$status" "caddy/caddy" "caddy runs"
case "$status" in *hugo/hugo* | *coredns/coredns*) fail "hugo or coredns started without their feature:\n$status" ;; esac
pass "hugo and coredns stay out"

http="$(cd "$CADDY" && zordon get module.caddy.service.go.caddy.vars.http)"
[ "$(http_get "http://127.0.0.1:$http/healthz")" = ok ] || fail "caddy /healthz"
page="$(curl -fsS --max-time 3 "http://127.0.0.1:$http/" || true)"
case "$page" in *zordon-hugo-ok*) fail "the hugo route exists without the hugo feature" ;; esac
pass "no hugo route without the feature"
