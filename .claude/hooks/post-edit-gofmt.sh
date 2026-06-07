#!/usr/bin/env bash
#
# Post-edit format gate for Claude Code (PostToolUse / Edit|Write|MultiEdit).
#
# After Claude touches a Go file, run `make fmt` so the working tree
# stays gofmt-clean without the model having to remember. Edits to
# non-Go files are a no-op (make fmt only touches *.go anyway).
#
# Claude passes the tool input as JSON on stdin; we read the edited
# path from .tool_input.file_path (same field for Edit, Write, and
# MultiEdit).
set -u

# Repo root. Claude sets CLAUDE_PROJECT_DIR for hooks; fall back to this
# script's own location (.claude/hooks/ -> two levels up).
repo="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

file="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
case "$file" in
	*.go) ;;
	*) exit 0 ;;
esac

# `make fmt` rewrites in place (gofmt -s -w + go fix ./...). On failure,
# feed the reason back to Claude (stderr is surfaced on exit 2) — a tree
# that won't fmt is usually a syntax error worth fixing right away.
if ! out="$(make -C "$repo" fmt 2>&1)"; then
	printf '\npost-edit gate: make fmt FAILED after editing %s:\n%s\n' "$file" "$out" >&2
	exit 2
fi
exit 0
