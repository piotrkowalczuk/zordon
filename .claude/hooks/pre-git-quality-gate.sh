#!/usr/bin/env bash
#
# Pre-git quality gate for Claude Code (PreToolUse / Bash matcher).
#
# Before a commit or push reaches git, run the project's checks and
# abort the git command (exit 2) the moment one fails:
#
#   git commit  ->  make lint + make test
#   git push    ->  make lint + make test
#
# Formatting is handled separately, on every Go edit
# (post-edit-gofmt.sh), so it is not repeated here.
#
# Claude passes the Bash command it is about to run as JSON on stdin;
# this script self-filters, so it also fires for git buried in a
# compound command (e.g. `make build && git push`).
set -u

# Repo root. Claude sets CLAUDE_PROJECT_DIR for hooks; fall back to this
# script's own location (.claude/hooks/ -> two levels up).
repo="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

cmd="$(jq -r '.tool_input.command // empty' 2>/dev/null)"
[ -n "$cmd" ] || exit 0

# True if $cmd invokes `git <sub>`, tolerating `-C dir` / `-c k=v`
# global options between `git` and the subcommand.
git_sub() {
	printf '%s' "$cmd" | grep -Eq \
		"(^|[^[:alnum:]_./-])git[[:space:]]+(-[A-Za-z][^[:space:]]*[[:space:]]+([^-][^[:space:]]*[[:space:]]+)?)*$1([[:space:]]|\$)"
}

op=
git_sub commit && op=commit
git_sub push && op=push

# Neither commit nor push -> not our business, let it through.
[ -n "$op" ] || exit 0

# Run one make target; on failure, explain to Claude (stderr is fed
# back on exit 2) and abort the git command.
gate() {
	printf '\n=== pre-git gate: make %s ===\n' "$1" >&2
	if ! make -C "$repo" "$1" >&2; then
		printf "\npre-git gate: 'make %s' FAILED -- aborting git %s.\n" "$1" "$op" >&2
		exit 2
	fi
}

# lint first (cheap, fails fast), then the heavy race+conformance suite.
gate lint
gate test

printf '\npre-git gate: all checks passed -- allowing git %s.\n' "$op" >&2
exit 0
