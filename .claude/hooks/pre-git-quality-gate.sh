#!/usr/bin/env bash
#
# Pre-git quality gate for Claude Code (PreToolUse / Bash matcher).
#
# Before a push reaches git, run the project's checks and abort the
# push (exit 2) the moment one fails:
#
#   git push  ->  make lint + make test
#
# Docs-only pushes (docs/**, *.md, mkdocs.yml) skip the gate — they
# can't break the Go build or tests.
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
git_sub push && op=push

# Not a push -> not our business, let it through.
[ -n "$op" ] || exit 0

# Docs-only pushes can't break `make lint`/`make test` (no Go compiled),
# so skip the gate for them. Err toward running: skip ONLY when every
# changed path is docs, and gate whenever the set is empty or undeterminable.
changed_files() {
	# files across the commits about to be pushed. Prefer the upstream
	# range; for a brand-new branch (no upstream yet) fall back to the diff
	# since the merge-base with origin/main, so a docs-only branch still
	# skips on its first push. Empty (-> gate) if neither resolves.
	range='@{upstream}..HEAD'
	if ! git -C "$repo" rev-parse --verify -q '@{upstream}' >/dev/null 2>&1; then
		base="$(git -C "$repo" merge-base origin/main HEAD 2>/dev/null)"
		[ -n "$base" ] && range="$base..HEAD"
	fi
	git -C "$repo" diff --name-only "$range" 2>/dev/null
}

is_docs_only() {
	local f seen=1
	while IFS= read -r f; do
		[ -n "$f" ] || continue
		seen=0
		case "$f" in
		docs/*|*.md|mkdocs.yml) ;;   # docs -> fine
		*) return 1 ;;               # anything else -> not docs-only
		esac
	done < <(changed_files)
	return "$seen"                   # 0 only if we saw >=1 file and all were docs
}

if is_docs_only; then
	printf '\npre-git gate: docs-only push -- skipping make lint/test.\n' >&2
	exit 0
fi

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
