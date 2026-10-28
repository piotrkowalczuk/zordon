#!/usr/bin/env bash
# Claim: a service whose git source follows a branch is locked to a commit.
# The first start records the commit the branch points at in zordon.lock and
# builds it; a later start keeps it when the branch moves; start --pull=srv
# moves it to the newest commit and says so; and a lock that holds an older
# commit, as one a teammate committed would, rebuilds the checkout there and
# status says how far it is behind. The repository is a local git repository
# that git reaches through url.<base>.insteadOf, so nothing leaves the machine.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

build_bins

# A fresh name per run: the bare clone under the zordon home is keyed by it.
REPO="github.com/zordon-example/hello-$$-$RANDOM"
ZHOME="${ZORDON_HOME:-$HOME/.zordon}"
TMP="$(mktemp -d)"
UP="$TMP/upstream"
PLACE="$TMP/place"
cleanup() {
	(cd "$PLACE" && zordon stop --agent >/dev/null 2>&1) || true
	pkill -f "$PLACE/" >/dev/null 2>&1 || true
	rm -rf "$TMP" "$ZHOME/src/github.com/zordon-example"
}
trap cleanup EXIT

commit_all() { # <message>
	git -C "$UP" add -A
	git -C "$UP" -c user.email=z@example.com -c user.name=zordon -c commit.gpgsign=false commit -q -m "$1"
	git -C "$UP" rev-parse HEAD
}

mkdir -p "$UP" "$PLACE"
cp -R hello "$UP/"
printf 'module %s\n\n%s\n' "$REPO" "$(grep '^go ' "$ROOT/go.mod")" >"$UP/go.mod"
git -C "$UP" init -q -b main
first="$(commit_all one)"

export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.file://$UP.insteadOf" GIT_CONFIG_VALUE_0="https://$REPO.git"

cat >"$PLACE/Alphasfile" <<ALPHASFILE
sysenv = ["HOME", "USER", "PATH", "LANG", "LC_ALL", "TZ", "TERM", "TMPDIR"]

toolchain {
  go {
    version = "1.27.0"
  }
}

# No branch, tag or rev: the service follows the default branch.
service "go" "hello" {
  git { url = "$REPO" }
  src { exe = "./hello" }

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

start_place() { # [zordon start flags...]
	info "zordon start${*:+ $*} in $PLACE"
	(cd "$PLACE" && zordon start --agent --timeout 600s --alpha-log "$PLACE/alpha.log" "$@" 2>&1)
}
hello() {
	local port
	port="$(cd "$PLACE" && zordon get service.go.hello.vars.port)" || fail "zordon get of the port failed"
	http_get "http://127.0.0.1:$port/" || fail "hello does not answer on $port"
}
status() { (cd "$PLACE" && zordon status --agent --format=text); }

start_place >/dev/null || fail "first start failed"
assert_contains "$(hello)" "hello one" "the service runs its git source"
assert_contains "$(tr -s ' \n' ' ' <"$PLACE/zordon.lock")" "source \"$REPO\" { ref = \"HEAD\" commit = \"$first\" }" "the first start locks the commit the default branch points at"

sed -i.bak 's/hello one/hello two/' "$UP/hello/main.go"
rm "$UP/hello/main.go.bak"
second="$(commit_all two)"

start_place >/dev/null || fail "start after the branch moved failed"
assert_contains "$(hello)" "hello one" "the lock keeps the service on its commit when the branch moves"

out="$(start_place --pull=srv)" || fail "start --pull=srv failed:\n$out"
assert_contains "$out" "zordon.lock $REPO@HEAD: ${first:0:12} -> ${second:0:12}" "--pull=srv reports the moved pin"
assert_contains "$(hello)" "hello two" "--pull=srv builds the newest commit"
assert_contains "$(cat "$PLACE/zordon.lock")" "commit = \"$second\"" "--pull=srv rewrites the lock"

sed -i.bak "s/$second/$first/" "$PLACE/zordon.lock"
rm "$PLACE/zordon.lock.bak"
start_place >/dev/null || fail "start from an older lock failed"
assert_contains "$(hello)" "hello one" "a lock holding an older commit rebuilds the checkout there"
assert_contains "$(status)" "behind HEAD by 1" "status says how far the pin is behind its ref"

pass "pull"
