#!/usr/bin/env bash
# Claim: a working place runs a package from another repository by its
# Go-style path. zordon pkg get pins the repository and locks its commit, the
# first start checks it out under the zordon home, the lock keeps the stack on
# that commit when the branch moves, and zordon pkg update moves the pin. The
# repository is a local git repository that git reaches through
# url.<base>.insteadOf, so nothing leaves the machine.
cd "$(dirname "$0")"
source ../_lib.sh
need curl

build_bins

# A fresh name per run: the bare clone and checkouts under the zordon home are
# keyed by it, and a new repository never shares history with an old one.
REPO="github.com/zordon-example/infra-$$-$RANDOM"
ZHOME="${ZORDON_HOME:-$HOME/.zordon}"
TMP="$(mktemp -d)"
UP="$TMP/upstream"
PLACE="$TMP/place"
cleanup() {
	(cd "$PLACE" && zordon stop --agent >/dev/null 2>&1) || true
	pkill -f "$PLACE/" >/dev/null 2>&1 || true
	rm -rf "$TMP" "$ZHOME/src/github.com/zordon-example" "$ZHOME/mod/github.com/zordon-example"
}
trap cleanup EXIT

commit_all() { # <message>
	git -C "$UP" add -A
	git -C "$UP" -c user.email=z@example.com -c user.name=zordon -c commit.gpgsign=false commit -q -m "$1"
	git -C "$UP" rev-parse HEAD
}

mkdir -p "$UP" "$PLACE"
cp -R infra/. "$UP/"
printf 'module %s\n\n%s\n' "$REPO" "$(grep '^go ' "$ROOT/go.mod")" >"$UP/go.mod"
git -C "$UP" init -q -b main
first="$(commit_all one)"

export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.file://$UP.insteadOf" GIT_CONFIG_VALUE_0="https://$REPO.git"

printf 'import "%s/hello" {}\n' "$REPO" >"$PLACE/Alphasfile"
if out="$(cd "$PLACE" && zordon --agent plan 2>&1)"; then
	fail "a remote import without a require must not plan:\n$out"
fi
assert_contains "$out" "run zordon pkg get $REPO@<ref>" "the missing require names the command that adds it"

out="$(cd "$PLACE" && zordon --agent pkg get "$REPO@main" 2>&1)" || fail "zordon pkg get failed:\n$out"
assert_contains "$(tr -s ' \n' ' ' <"$PLACE/Alphasfile")" "require \"$REPO\" { ref = \"main\" }" "pkg get writes the require into the Alphasfile"
assert_contains "$(cat "$PLACE/zordon.lock")" "commit = \"$first\"" "pkg get locks the commit main points at"

start_place() {
	info "zordon start in $PLACE"
	(cd "$PLACE" && zordon start --agent --timeout 600s --alpha-log "$PLACE/alpha.log")
}
hello() {
	local port
	port="$(cd "$PLACE" && zordon get package.hello.component.hello.service.go.hello.vars.port)" || fail "zordon get of the port failed"
	http_get "http://127.0.0.1:$port/" || fail "hello does not answer on $port"
}

start_place
assert_contains "$(hello)" "hello one" "the package runs from the fetched repository"
assert_present "$ZHOME/mod/$REPO@$first"
status="$(cd "$PLACE" && zordon status --agent --format=text)"
assert_contains "$status" "@${first:0:12}" "status shows the locked commit"
(cd "$PLACE" && zordon stop --agent >/dev/null 2>&1) || true

sed -i.bak 's/hello one/hello two/' "$UP/hello/main.go"
rm "$UP/hello/main.go.bak"
second="$(commit_all two)"

plan="$(cd "$PLACE" && zordon --agent plan)" || fail "zordon plan failed after main moved"
assert_contains "$plan" "$REPO@${first:0:12}" "the lock keeps the stack on its commit when the branch moves"

out="$(cd "$PLACE" && zordon --agent pkg update 2>&1)" || fail "zordon pkg update failed:\n$out"
assert_contains "$out" "$REPO@main: ${first:0:12} -> ${second:0:12}" "pkg update reports the moved pin"
assert_contains "$(cat "$PLACE/zordon.lock")" "commit = \"$second\"" "pkg update rewrites the lock"

start_place
assert_contains "$(hello)" "hello two" "the next start runs the new commit"
