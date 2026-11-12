#!/usr/bin/env bash
# Claim:
#   1. (#140) With a cold ZORDON_HOME and no cargo/cmake required, zordon
#      bootstraps mise from the pinned prebuilt release archive (SHA-256
#      verified) in seconds rather than compiling from source.
#   2. (#137) `zordon start` streams toolchain bootstrap and `mise install`
#      output live under `toolchain/<name>`, and when a toolchain install
#      fails, surfaces `toolchain/<name>` with its captured output tail in the
#      failure summary instead of opaque `dep ... failed` service fallout.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need git
need go
need_net

COLD_HOME="$EXROOT/.zordon/cold-home"
FAIL_DIR="$(mktemp -d "${TMPDIR:-/tmp}/zordon-tc-fail.XXXXXX")"
SHIM_BIN="$EXROOT/.zordon/shim-bin"

cleanup() {
	ZORDON_HOME="$COLD_HOME" zordon stop --agent >/dev/null 2>&1 || true
	( cd "$FAIL_DIR" 2>/dev/null && ZORDON_HOME="$COLD_HOME" zordon stop --agent >/dev/null 2>&1 ) || true
	reap
	rm -rf "$COLD_HOME" "$FAIL_DIR" "$SHIM_BIN"
}
trap cleanup EXIT

build_bins
( cd "$ROOT" && go build -o bin/tommy ./cmd/tommy/ )

reset_state main
rm -rf "$COLD_HOME" "$SHIM_BIN"
mkdir -p "$COLD_HOME" "$SHIM_BIN" "$(dirname "$ALPHA_LOG")"

for tool in git go curl; do
	ln -sf "$(command -v "$tool")" "$SHIM_BIN/$tool"
done

export ZORDON_HOME="$COLD_HOME"
export PATH="$ROOT/bin:$SHIM_BIN:/usr/bin:/bin"

info "zordon start with cold ZORDON_HOME (prebuilt mise release bootstrap + live streaming)"
zordon start --agent --timeout 120s --alpha-log "$ALPHA_LOG" 2>&1 | tee "$ZORDON_LOG"

assert_present "$COLD_HOME/bin/mise"
assert_present "$COLD_HOME/bin/.mise-version"
assert_zordon_contains "downloading mise v" "mise release download streamed to zordon start"
assert_zordon_contains "bootstrapped mise v" "mise prebuilt archive verified and extracted"
assert_zordon_contains "toolchain/pkg" "toolchain output attributed to toolchain/pkg"

stamp="$EXROOT/workspaces/main/stamp.json"
assert_present "$stamp"
assert_contains "$(cat "$stamp")" '{"bootstrapped":true}' "jq installed via bootstrapped mise produced stamp.json"

zordon stop --agent >/dev/null 2>&1 || true

info "verifying --mise.version flag validation"
if bad_ver_out="$(zordon start --agent --mise.version=latest 2>&1)"; then
	fail "expected --mise.version=latest to be rejected"
fi
assert_contains "$bad_ver_out" "invalid mise version" "--mise.version rejects unpinned/malformed version"

info "verifying toolchain failure summary surfaces culprit output (#137)"
mkdir -p "$FAIL_DIR"
cat >"$FAIL_DIR/Alphasfile" <<EOF
sysenv = ["HOME", "USER", "PATH", "LANG", "LC_ALL", "TZ", "TERM", "TMPDIR"]

toolchain {
  go {
    version = "0.0.0-nonexistent"
  }
}

service "go" "api" {
  src {
    path = "$ROOT"
  }
  runtime {
    cmd = ["/bin/sh", "-c", "sleep 30"]
  }
}
EOF

FAIL_LOG="$FAIL_DIR/zordon-fail.log"
if ( cd "$FAIL_DIR" && zordon start --agent --timeout 60s --alpha-log "$FAIL_DIR/alpha.log" >"$FAIL_LOG" 2>&1 ); then
	fail "expected zordon start with nonexistent toolchain version to fail"
fi

fail_out="$(cat "$FAIL_LOG")"
assert_contains "$fail_out" "Bringup failed: 1 service(s)" "single root-cause failure reported (no secondary service fallout)"
assert_contains "$fail_out" "[toolchain/go] error:" "toolchain/go reported as culprit"
assert_contains "$fail_out" "[toolchain/go] last" "captured mise failure tail printed in summary"
