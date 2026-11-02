#!/usr/bin/env bash
# Claim: a service that names a checkout beside its git source runs the git
# source while the checkout is absent and the checkout once it is there. The
# start log and status say when git stands in, and the lock reads the same
# either way. The git source is a local git repository that git reaches
# through url.<base>.insteadOf, so nothing leaves the machine.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

build_bins

# A fresh name per run: the bare clone and the module checkouts under the
# zordon home are keyed by it.
REPO="github.com/zordon-example/hello-$$-$RANDOM"
ZHOME="${ZORDON_HOME:-$HOME/.zordon}"
TMP="$(mktemp -d)"
UP="$TMP/upstream"
PLACE="$TMP/place"
CHECKOUT="$TMP/checkout"
cleanup() {
	(cd "$PLACE" && zordon stop --agent >/dev/null 2>&1) || true
	pkill -f "$PLACE/" >/dev/null 2>&1 || true
	rm -rf "$TMP" "$ZHOME/src/github.com/zordon-example" "$ZHOME/mod/github.com/zordon-example"
}
trap cleanup EXIT

mkdir -p "$UP" "$PLACE"
cp -R hello "$UP/"
printf 'module %s\n\n%s\n' "$REPO" "$(grep '^go ' "$ROOT/go.mod")" >"$UP/go.mod"
git -C "$UP" init -q -b main
git -C "$UP" add -A
git -C "$UP" -c user.email=z@example.com -c user.name=zordon -c commit.gpgsign=false commit -q -m one
first="$(git -C "$UP" rev-parse HEAD)"

export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.file://$UP.insteadOf" GIT_CONFIG_VALUE_0="https://$REPO.git"

cat >"$PLACE/Alphasfile" <<ALPHASFILE
sysenv = ["HOME", "USER", "PATH", "LANG", "LC_ALL", "TZ", "TERM", "TMPDIR"]

toolchain {
  go {
    version = "1.27.0"
  }
}

# The checkout is preferred while it is there; the git source stands in
# while it is not.
service "go" "hello" {
  git { url = "$REPO" }
  src {
    path = "$CHECKOUT"
    exe  = "./hello"
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["\${fs::bin()}/hello", "-addr", "127.0.0.1:\${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
ALPHASFILE

start_place() {
	info "zordon start in $PLACE"
	(cd "$PLACE" && zordon start --agent --timeout 600s --alpha-log "$PLACE/alpha.log" 2>&1)
}
hello() {
	local port
	port="$(cd "$PLACE" && zordon get service.go.hello.vars.port)" || fail "zordon get of the port failed"
	http_get "http://127.0.0.1:$port/" || fail "hello does not answer on $port"
}
lock() { tr -s ' \n' ' ' <"$PLACE/zordon.lock"; }
# The place's own level: a federation root above it has pins of its own.
status() { (cd "$PLACE" && zordon status --agent --format=text) | sed -n '/^\.\/Alphasfile/,$p'; }
assert_lacks() { # <haystack> <needle> [label]
	if [[ "$1" == *"$2"* ]]; then fail "${3:-unexpected $2}:\n$1"; fi
	pass "${3:-no $2}"
}

out="$(start_place)" || fail "start without the checkout failed:\n$out"
assert_contains "$(hello)" "hello one" "the git source runs while the checkout is absent"
assert_contains "$(cat "$PLACE/alpha.log")" "src $CHECKOUT is missing; $REPO stands in" "the start says the git source stands in for the missing checkout"
assert_contains "$(status)" "src $CHECKOUT missing" "status names the missing checkout"
assert_contains "$(lock)" "source \"$REPO\" { ref = \"HEAD\" commit = \"$first\" }" "the lock pins the git source"

git clone -q "$UP" "$CHECKOUT"
sed -i.bak 's/hello one/hello local/' "$CHECKOUT/hello/main.go"
rm "$CHECKOUT/hello/main.go.bak"
out="$(start_place)" || fail "start with the checkout present failed:\n$out"
assert_contains "$(hello)" "hello local" "the checkout is preferred over the git source once it is there"
assert_contains "$(status)" "(branch main)" "status shows the checkout in place, on its own branch"
assert_lacks "$(status)" "missing" "status names no missing checkout once it is there"
assert_contains "$(lock)" "source \"$REPO\" { ref = \"HEAD\" commit = \"$first\" }" "the lock reads the same with the checkout there"

pass "src_git"
