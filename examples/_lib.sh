# shellcheck shell=bash
# Shared harness for examples/<name>/test.sh.
#
# Each test.sh sources this, calls `start`, asserts the claim the
# example is supposed to demonstrate, and returns nonzero on failure.
# A missing prerequisite (no network, no sudo, missing toolchain) is a
# SKIP (exit 0) so `make e2e` keeps going.

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
ZORDON="$ROOT/bin/zordon"
ALPHA="$ROOT/bin/alpha"

# Sourced right after `cd "$(dirname "$0")"`, so this is the example
# root (examples/<name>) even for worktree, which cds deeper later.
# Per-example binaries live under "$EXROOT/.zordon"; port discovery is
# scoped to it so a same-named binary from another example (several are
# just called `app`) or a stale prior run can't be matched by mistake.
EXROOT="$PWD"

c_red=$'\033[31m'; c_grn=$'\033[32m'; c_yel=$'\033[33m'; c_rst=$'\033[0m'
info() { echo "${c_yel}··${c_rst} $*" >&2; }
pass() { echo "${c_grn}OK${c_rst} $*" >&2; }
fail() { echo "${c_red}FAIL${c_rst} $*" >&2; exit 1; }
skip() { echo "${c_yel}SKIP${c_rst} $*" >&2; exit 0; }

need() { command -v "$1" >/dev/null 2>&1 || skip "missing tool: $1"; }

need_net() {
	curl -fsS --head --max-time 5 https://github.com >/dev/null 2>&1 \
		|| skip "no network (example fetches sources from git)"
}

# need_sudo: federation mutates /etc (pf, resolver). Only run when sudo
# is non-interactive; otherwise skip rather than block on a prompt.
need_sudo() {
	[ "$(id -u)" = 0 ] && return 0
	sudo -n true >/dev/null 2>&1 || skip "passwordless sudo unavailable"
}

build_bins() {
	# Always rebuild: a stale alpha silently runs old resolution logic.
	# `go build` is incremental, so this is cheap when nothing changed.
	info "building zordon + alpha"
	( cd "$ROOT" && mkdir -p bin \
		&& go build -o bin/zordon ./cmd/zordon/ \
		&& go build -o bin/alpha ./cmd/alpha/ )
}

# alpha is resolved by name off $PATH; keep the freshly built one first.
export PATH="$ROOT/bin:$PATH"
zordon() { "$ZORDON" "$@"; }

# reap: kill any process still bound to THIS example's state dir (a
# leaked detached alpha/service from an earlier interrupted run).
reap() { pkill -f "$EXROOT/.zordon/" >/dev/null 2>&1 || true; }

# reset_state: clean this example's zordon-managed scratch so the run is
# reproducible. Removes its .zordon dir, prunes dead git worktrees, and
# deletes the legacy bare `zordon/<wt>` branch (pre per-service-branch
# scheme) which otherwise blocks creating `zordon/<wt>/<svc>`. Scoped to
# the zordon/* namespace — never touches user branches.
reset_state() {
	local wt="${1:-main}" repo b
	reap
	rm -rf "$EXROOT/.zordon/worktrees/$wt"
	# The user's repo (dir primaries) AND every cached bare clone (git
	# primaries, ~/.zordon/src/**.git) may carry a legacy bare
	# `zordon/<wt>` branch that blocks creating `zordon/<wt>/<svc>`.
	for repo in "$ROOT" $(find "$HOME/.zordon/src" -type d -name '*.git' 2>/dev/null); do
		git -C "$repo" worktree prune >/dev/null 2>&1 || true
		for b in $(git -C "$repo" for-each-ref --format='%(refname:short)' \
			"refs/heads/zordon/$wt" 2>/dev/null); do
			git -C "$repo" branch -D "$b" >/dev/null 2>&1 || true
		done
	done
}

# start: bring the stack up; auto-stop on exit.
#
# alpha-log is pinned to <example>/.zordon/alpha.log so tests can assert
# against it without colliding with other examples or stale runs in
# /tmp/alpha.log. reset_state wipes .zordon/ first so the file starts empty.
ALPHA_LOG="$EXROOT/.zordon/alpha.log"
start() {
	build_bins
	trap 'zordon stop --agent >/dev/null 2>&1 || true; reap' EXIT
	reset_state main
	mkdir -p "$(dirname "$ALPHA_LOG")"
	info "zordon start"
	zordon start --agent --timeout 90s --alpha-log "$ALPHA_LOG"
}

# port_of <argv-substring>: first `-addr 127.0.0.1:<port>` of a running
# process whose argv contains the substring. Our example binaries are
# launched as `<bin> -addr 127.0.0.1:<port> ...`.
port_of() {
	local pat="$1" line
	# Scope to this example's state dir first; fall back to a bare match
	# (ruby is launched as `ruby .../app.rb`, not from "$EXROOT/.zordon").
	line="$(ps -axww -o args= 2>/dev/null | grep -F "$EXROOT/.zordon" | grep -F -- "$pat" | grep -v grep | head -1 || true)"
	[ -n "$line" ] || line="$(ps -axww -o args= 2>/dev/null | grep -F -- "$pat" | grep -v grep | head -1 || true)"
	[ -n "$line" ] || return 1
	echo "$line" | sed -nE 's/.*-addr 127\.0\.0\.1:([0-9]+).*/\1/p'
}

# http_get <url>: curl with a short retry window.
http_get() {
	local url="$1" i out
	for i in $(seq 1 30); do
		if out="$(curl -fsS --max-time 3 "$url" 2>/dev/null)"; then
			printf '%s' "$out"; return 0
		fi
		sleep 0.3
	done
	return 1
}

assert_contains() { # <haystack> <needle> [label]
	case "$1" in
		*"$2"*) pass "${3:-assertion}: contains '$2'";;
		*) fail "${3:-assertion}: expected to contain '$2', got: $1";;
	esac
}

# assert_log_contains <needle> [label]: tail of the alpha log file
# (per-example, set by `start`) must contain <needle>. Used to verify
# service stdout actually made it through alpha without being eaten by
# userspace buffering — the failure mode that motivated the PTY work.
assert_log_contains() {
	local needle="$1" label="${2:-alpha log}" log
	[ -r "$ALPHA_LOG" ] || fail "$label: alpha log not found at $ALPHA_LOG"
	log="$(cat "$ALPHA_LOG")"
	case "$log" in
		*"$needle"*) pass "$label: contains '$needle'";;
		*) fail "$label: expected to contain '$needle'. Log tail:\n$(tail -40 "$ALPHA_LOG")";;
	esac
}

assert_absent() { # <path>
	[ ! -e "$1" ] && pass "absent: $1" || fail "expected NOT to exist: $1"
}

assert_present() { # <path>
	[ -e "$1" ] && pass "present: $1" || fail "expected to exist: $1"
}
