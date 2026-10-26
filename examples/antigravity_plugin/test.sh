#!/usr/bin/env bash
# Claim: the Antigravity plugin hooks (`hooks/zordon-hook.sh`) and sidecar UI
# (`sidecars/dashboard/server.py`) report live stack status, record MCP tool
# calls into shared state, and serve the auxiliary dashboard over HTTP.
cd "$(dirname "$0")"
source ../_lib.sh
need curl
need python3
need jq

start
port="$(port_of "/app -addr")" || fail "could not discover app port"
body="$(http_get "http://127.0.0.1:$port/")" || fail "endpoint not responding"
assert_contains "$body" "antigravity-plugin ok" "service body"

state_file="$(mktemp -t zordon-agy-state.XXXXXX)"
sidecar_err="$(mktemp -t zordon-agy-sidecar.XXXXXX)"
sidecar_pid=""
cleanup() {
	[ -n "$sidecar_pid" ] && kill "$sidecar_pid" 2>/dev/null || true
	rm -f "$state_file" "$sidecar_err"
	zordon stop --agent >/dev/null 2>&1 || true
	reap
}
trap cleanup EXIT

export ZORDON_BIN="$ZORDON"
export ZORDON_AGY_STATE="$state_file"

# 1. SessionStart hook injects live stack summary and sidecar pill link.
session_out="$(jq -cn --arg ws "$EXROOT" '{conversationId:"e2e-1", workspacePaths:[$ws]}' \
	| "$ROOT/hooks/zordon-hook.sh" session-start)"
assert_contains "$session_out" "zordon 1/1 up" "SessionStart summary"
assert_contains "$session_out" "http://127.0.0.1:$port/" "SessionStart print URL"
assert_contains "$session_out" "sidecar://zordon/dashboard/" "SessionStart sidecar pill"

# 2. PreToolUse + PostToolUse record an MCP tool call into shared state.
pre_out="$(jq -cn --arg ws "$EXROOT" '{
	conversationId: "e2e-1",
	workspacePaths: [$ws],
	stepIdx: 1,
	toolCall: {
		name: "call_mcp_tool",
		args: {
			ServerName: "zordon_zordon",
			ToolName: "status",
			Arguments: {args: ["--format=json"]}
		}
	}
}' | "$ROOT/hooks/zordon-hook.sh" pre-tool)"
assert_contains "$pre_out" '"decision":"allow"' "PreToolUse allow decision"

post_out="$(jq -cn --arg ws "$EXROOT" '{
	conversationId: "e2e-1",
	workspacePaths: [$ws],
	stepIdx: 1,
	output: "workspace=main state=running",
	toolCall: {
		name: "call_mcp_tool",
		args: {
			ServerName: "zordon_zordon",
			ToolName: "status",
			Arguments: {args: ["--format=json"]}
		}
	}
}' | "$ROOT/hooks/zordon-hook.sh" post-tool)"
[ "$post_out" = "{}" ] && pass "PostToolUse empty object response" || fail "PostToolUse got: $post_out"

# 3. Sidecar HTTP server serves the live snapshot and recorded MCP call log.
ANTIGRAVITY_SIDECAR_WEB_PORT=0 python3 "$ROOT/sidecars/dashboard/server.py" 2>"$sidecar_err" &
sidecar_pid=$!

sidecar_url=""
for _ in $(seq 1 30); do
	if [ -s "$sidecar_err" ]; then
		sidecar_url="$(sed -nE 's/.*listening on (http:\/\/127\.0\.0\.1:[0-9]+\/).*/\1/p' "$sidecar_err" | head -1)"
		[ -n "$sidecar_url" ] && break
	fi
	sleep 0.2
done
[ -n "$sidecar_url" ] || fail "sidecar did not announce listen URL: $(cat "$sidecar_err")"

snap="$(http_get "${sidecar_url}api/snapshot")" || fail "sidecar /api/snapshot failed"
assert_contains "$snap" '"summary": "zordon 1/1 up"' "sidecar snapshot summary"
assert_contains "$snap" "http://127.0.0.1:$port/" "sidecar snapshot service print"
assert_contains "$snap" '"command": "status"' "sidecar snapshot recorded MCP call"
