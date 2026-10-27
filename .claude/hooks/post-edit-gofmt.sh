#!/usr/bin/env bash
#
# Post-edit format gate for Claude Code (Edit|Write|MultiEdit) and
# Antigravity (edit_file|write_file).
#
# After the agent touches a Go file, run `make fmt` so the working tree
# stays gofmt-clean without the model having to remember. Edits to
# non-Go files are a no-op (make fmt only touches *.go anyway).
set -u

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
# shellcheck source=../../hooks/adapter.sh
source "$root/hooks/adapter.sh"

hook_read_stdin
file="$(hook_file_path)"
case "$file" in
	*.go) ;;
	*) hook_post_ok ;;
esac

cwd="$(hook_cwd)"
repo="${CLAUDE_PROJECT_DIR:-}"
if [ -z "$repo" ] && [ -n "$cwd" ]; then
	repo="$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null || true)"
fi
[ -n "$repo" ] || repo="$root"

# `make fmt` rewrites in place (gofmt -s -w + go fix ./...). On failure,
# feed the reason back to the agent (stderr is surfaced on exit 2) — a tree
# that won't fmt is usually a syntax error worth fixing right away.
if ! out="$(make -C "$repo" fmt 2>&1)"; then
	hook_post_fail "$(printf 'post-edit gate: make fmt FAILED after editing %s:\n%s' "$file" "$out")"
fi
hook_post_ok
