#!/usr/bin/env bash
# Checks which checkout pre-git-quality-gate.sh gates: `make` is replaced by a
# stub that records its -C dir, so no real lint/test runs.
#
#   bash .claude/hooks/pre-git-quality-gate_test.sh
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hook="$here/pre-git-quality-gate.sh"
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
exit 0
STUB
chmod +x "$tmp/bin/make"

fails=0
gated() { # <expected repo or "none"> <cwd> <command> [CLAUDE_PROJECT_DIR]
	local want="$1" cwd="$2" cmd="$3" proj="${4:-/nonexistent}" got
	: > "$tmp/make.log"
	jq -n --arg c "$cmd" --arg d "$cwd" '{tool_input: {command: $c}, cwd: $d}' |
		PATH="$tmp/bin:$PATH" MAKE_LOG="$tmp/make.log" CLAUDE_PROJECT_DIR="$proj" \
			bash "$hook" 2>/dev/null
	got="$(sort -u "$tmp/make.log")"
	[ -n "$got" ] || got=none
	if [ "$got" = "$want" ]; then
		printf 'ok   %s\n' "$cmd"
	else
		printf 'FAIL %s (cwd=%s): gated %s, want %s\n' "$cmd" "$cwd" "$got" "$want"
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

[ "$fails" -eq 0 ] || exit 1
