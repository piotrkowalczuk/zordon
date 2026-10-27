#!/usr/bin/env bash
# Checks which checkout pre-git-quality-gate.sh and post-edit-gofmt.sh gate
# across both Claude Code and Antigravity payloads: `make` is replaced by a
# stub that records its -C dir, so no real lint/test runs.
#
#   bash .claude/hooks/pre-git-quality-gate_test.sh
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hook="$here/pre-git-quality-gate.sh"
fmt_hook="$here/post-edit-gofmt.sh"
root="$(git -C "$here" rev-parse --show-toplevel)"

tmp="$(mktemp -d)"
sub="$(mktemp -d)"
trap 'rm -rf "$tmp" "$sub"' EXIT
mkdir -p "$tmp/bin" "$sub/nested"
git -C "$sub" init -q
subtop="$(git -C "$sub" rev-parse --show-toplevel)" # /var -> /private/var on macOS
cat > "$tmp/bin/make" <<'STUB'
#!/usr/bin/env bash
[ "$1" = -C ] && printf '%s\n' "$2" >> "$MAKE_LOG"
exit "${MAKE_EXIT:-0}"
STUB
chmod +x "$tmp/bin/make"

fails=0
gated() { # <expected repo or "none"> <cwd> <command> [CLAUDE_PROJECT_DIR]
	local want="$1" cwd="$2" cmd="$3" proj="${4:-/nonexistent}" got out

	# 1. Claude Code payload (.tool_input.command)
	: > "$tmp/make.log"
	jq -n --arg c "$cmd" --arg d "$cwd" '{tool_input: {command: $c}, cwd: $d}' |
		PATH="$tmp/bin:$PATH" MAKE_LOG="$tmp/make.log" CLAUDE_PROJECT_DIR="$proj" \
			bash "$hook" 2>/dev/null
	got="$(sort -u "$tmp/make.log")"
	[ -n "$got" ] || got=none
	if [ "$got" = "$want" ]; then
		printf 'ok   [claude]      %s\n' "$cmd"
	else
		printf 'FAIL [claude]      %s (cwd=%s): gated %s, want %s\n' "$cmd" "$cwd" "$got" "$want"
		fails=$((fails + 1))
	fi

	# 2. Antigravity payload (.toolCall.args.CommandLine + .workspacePaths)
	: > "$tmp/make.log"
	out="$(jq -n --arg c "$cmd" --arg d "$cwd" \
		'{conversationId: "conv-1", workspacePaths: (if $d == "" then [] else [$d] end), toolCall: {name: "run_command", args: {CommandLine: $c}}}' |
		PATH="$tmp/bin:$PATH" MAKE_LOG="$tmp/make.log" CLAUDE_PROJECT_DIR="$proj" \
			bash "$hook" 2>/dev/null)"
	got="$(sort -u "$tmp/make.log")"
	[ -n "$got" ] || got=none
	decision="$(printf '%s' "$out" | jq -r '.decision // empty' 2>/dev/null)"
	if [ "$got" = "$want" ] && [ "$decision" = "allow" ]; then
		printf 'ok   [antigravity] %s\n' "$cmd"
	else
		printf 'FAIL [antigravity] %s (cwd=%s): gated %s (want %s), decision=%s (want allow)\n' "$cmd" "$cwd" "$got" "$want" "$decision"
		fails=$((fails + 1))
	fi
}

p=push
gated "$root" "$root" "git $p"
gated "$root" "$root/.claude/hooks" "git $p origin HEAD"
gated "$subtop" "$root" "git -C $sub $p"
gated "$subtop" "$root" "git -C \"$sub\" $p -u origin x"
gated "$subtop" "$sub/nested" "make build && git $p"
gated "$root" "" "git $p" "$root"
gated none "$root" "git status"
gated none "$root" "echo git $p-ups"

# Verify Antigravity deny JSON when make fails.
deny_out="$(jq -n --arg c "git $p" --arg d "$root" \
	'{conversationId: "conv-1", workspacePaths: [$d], toolCall: {name: "run_command", args: {CommandLine: $c}}}' |
	PATH="$tmp/bin:$PATH" MAKE_LOG="$tmp/make.log" MAKE_EXIT=1 \
		bash "$hook" 2>/dev/null)"
if [ "$(printf '%s' "$deny_out" | jq -r '.decision')" = "deny" ]; then
	printf 'ok   [antigravity] deny decision on make failure\n'
else
	printf 'FAIL [antigravity] expected deny decision, got: %s\n' "$deny_out"
	fails=$((fails + 1))
fi

# Verify post-edit-gofmt.sh with Antigravity payload on a .go file.
: > "$tmp/make.log"
fmt_out="$(jq -n --arg f "$sub/main.go" --arg d "$sub" \
	'{conversationId: "conv-1", workspacePaths: [$d], toolCall: {name: "edit_file", args: {Path: $f}}}' |
	PATH="$tmp/bin:$PATH" MAKE_LOG="$tmp/make.log" \
		bash "$fmt_hook" 2>/dev/null)"
if [ "$(sort -u "$tmp/make.log")" = "$subtop" ] && [ "$fmt_out" = "{}" ]; then
	printf 'ok   [antigravity] post-edit-gofmt.sh runs make fmt and returns {}\n'
else
	printf 'FAIL [antigravity] post-edit-gofmt.sh: log=%s out=%s\n' "$(cat "$tmp/make.log")" "$fmt_out"
	fails=$((fails + 1))
fi

[ "$fails" -eq 0 ] || exit 1
