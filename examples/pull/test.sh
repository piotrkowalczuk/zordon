#!/usr/bin/env bash
# Claim: what a working place takes from git is locked to a commit, and
# zordon start --pull moves the lock. The place runs a service whose git
# source follows a branch and imports a package from a repository it
# requires. The first start records the commit each one points at in
# zordon.lock and builds it; a later start keeps both when the branch moves;
# --pull=pkg moves the required repository and leaves the service's source,
# --pull=srv the other way round, --pull=all both, each saying so; status says
# how far a pin is behind the ref its repository last fetched; and a lock that
# holds other commits, as one a teammate committed would, rebuilds there,
# whether the bare clone has fetched them or not. The service's source and the
# required repository are one local git repository that git reaches through
# url.<base>.insteadOf, so nothing leaves the machine.
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
bump() { # <from> <to>: a commit where both programs answer with <to>
	local f
	for f in hello greet; do
		sed -i.bak "s/$f $1/$f $2/" "$UP/$f/main.go"
		rm "$UP/$f/main.go.bak"
	done
	commit_all "$2"
}

mkdir -p "$UP" "$PLACE"
cp -R hello greet "$UP/"
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

# The package follows main; the lock holds the commit.
require "$REPO" { ref = "main" }

import "$REPO/greet" {}

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
answer() { # <port expression>: what GET / on that port says
	local port
	port="$(cd "$PLACE" && zordon get "$1")" || fail "zordon get $1 failed"
	http_get "http://127.0.0.1:$port/" || fail "nothing answers on $port"
}
hello() { answer service.go.hello.vars.port; }
greet() { answer package.greet.component.greet.service.go.greet.vars.port; }
lock() { tr -s ' \n' ' ' <"$PLACE/zordon.lock"; }
# The place's own level: a federation root above it has pins of its own.
status() { (cd "$PLACE" && zordon status --agent --format=text) | sed -n '/^\.\/Alphasfile/,$p'; }
assert_lacks() { # <haystack> <needle> [label]
	if [[ "$1" == *"$2"* ]]; then fail "${3:-unexpected $2}:\n$1"; fi
	pass "${3:-no $2}"
}

start_place >/dev/null || fail "first start failed"
assert_contains "$(hello)" "hello one" "the service runs its git source"
assert_contains "$(greet)" "greet one" "the package runs from the required repository"
assert_contains "$(lock)" "source \"$REPO\" { ref = \"HEAD\" commit = \"$first\" }" "the first start locks the commit the service's branch points at"
assert_contains "$(lock)" "repo \"$REPO\" { ref = \"main\" commit = \"$first\" }" "the first start locks the commit the required repository's branch points at"

second="$(bump one two)"

start_place >/dev/null || fail "start after the branch moved failed"
assert_contains "$(hello)" "hello one" "the lock keeps the service on its commit when the branch moves"
assert_contains "$(greet)" "greet one" "the lock keeps the package on its commit when the branch moves"

out="$(start_place --pull=pkg)" || fail "start --pull=pkg failed:\n$out"
assert_contains "$out" "zordon.lock $REPO@main: ${first:0:12} -> ${second:0:12}" "--pull=pkg reports the moved repository pin"
assert_contains "$(greet)" "greet two" "--pull=pkg builds the required repository's newest commit"
assert_contains "$(hello)" "hello one" "--pull=pkg leaves the service's source where it was"
assert_contains "$(lock)" "source \"$REPO\" { ref = \"HEAD\" commit = \"$first\" }" "--pull=pkg leaves the source pin in the lock"
assert_contains "$(status)" "behind HEAD by 1" "status says the service's source is behind the ref --pull=pkg fetched"

out="$(start_place --pull=srv)" || fail "start --pull=srv failed:\n$out"
assert_contains "$out" "zordon.lock $REPO@HEAD: ${first:0:12} -> ${second:0:12}" "--pull=srv reports the moved source pin"
assert_contains "$(hello)" "hello two" "--pull=srv builds the service's newest commit"
assert_contains "$(lock)" "source \"$REPO\" { ref = \"HEAD\" commit = \"$second\" }" "--pull=srv rewrites the lock"
assert_lacks "$(status)" "behind" "status says nothing once both pins are at the fetched ref"

third="$(bump two three)"

out="$(start_place --pull=all)" || fail "start --pull=all failed:\n$out"
assert_contains "$out" "zordon.lock $REPO@main: ${second:0:12} -> ${third:0:12}" "--pull=all reports the moved repository pin"
assert_contains "$out" "zordon.lock $REPO@HEAD: ${second:0:12} -> ${third:0:12}" "--pull=all reports the moved source pin"
assert_contains "$(hello)" "hello three" "--pull=all builds the service's newest commit"
assert_contains "$(greet)" "greet three" "--pull=all builds the required repository's newest commit"

sed -i.bak "s/$third/$second/g" "$PLACE/zordon.lock"
rm "$PLACE/zordon.lock.bak"
start_place >/dev/null || fail "start from an older lock failed"
assert_contains "$(hello)" "hello two" "a lock holding an older commit rebuilds the service there"
assert_contains "$(greet)" "greet two" "a lock holding an older commit rebuilds the package there"
st="$(status)"
assert_contains "$st" "behind HEAD by 1" "status says how far the service's source is behind its ref"
assert_contains "$st" "behind main by 1" "status says how far the required repository is behind its ref"

fourth="$(bump three four)"
sed -i.bak "s/$second/$fourth/g" "$PLACE/zordon.lock"
rm "$PLACE/zordon.lock.bak"
out="$(start_place)" || fail "start from a lock ahead of the bare clone failed:\n$out"
assert_contains "$(hello)" "hello four" "a lock holding a commit nothing here fetched yet, as a teammate's would, checks the service out there"
assert_contains "$(greet)" "greet four" "a lock holding a commit nothing here fetched yet checks the package out there"
assert_lacks "$out" "cannot tell how far" "a pin the bare clone has not fetched is left out quietly"
assert_lacks "$(status)" "behind" "status says nothing of a pin the bare clone has not fetched"

pass "pull"
