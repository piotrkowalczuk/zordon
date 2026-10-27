#!/usr/bin/env sh
# Fail loudly, at session start, when the host-side zordon endpoint is not
# reachable. Without this the agent just sees an MCP server that never
# connected and no zordon tools — a silence that reads like "zordon is broken"
# rather than "nobody started it on the host".
#
# Shared by Claude Code (.claude/settings.json -> plain text on stdout) and
# Antigravity (.agents/hooks.json -> {"injectSteps":[...]} JSON on stdout).
set -eu

input=""
if [ ! -t 0 ]; then
	input="$(cat || true)"
fi

emit() {
	msg="$1"
	case "$input" in
		*'"conversationId"'*|*'"workspacePaths"'*)
			if command -v jq >/dev/null 2>&1; then
				jq -cn --arg m "$msg" '{injectSteps: [{systemMessage: {systemMessage: $m}}]}'
			else
				esc="$(printf '%s' "$msg" | sed 's/\\/\\\\/g; s/"/\\"/g' | awk '{printf "%s\\n", $0}' | sed 's/\\n$//')"
				printf '{"injectSteps":[{"systemMessage":{"systemMessage":"%s"}}]}\n' "$esc"
			fi
			;;
		*)
			printf '%s\n' "$msg"
			;;
	esac
}

URL="${ZORDON_MCP_URL:-http://host.docker.internal:7391/mcp}"

if curl -fs -o /dev/null --max-time 5 \
	-X POST "$URL" \
	-H 'Content-Type: application/json' \
	-H 'Accept: application/json, text/event-stream' \
	-d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'; then
	emit "zordon MCP reachable at $URL — use the zordon tools to inspect and drive the stack.
The stack may not be up yet; call the start tool if you need it running."
	exit 0
fi

emit "$(cat <<EOF
zordon MCP is NOT reachable at $URL.

The MCP server runs on the host, not in this container. Run there:

    zordon mcp --transport=http --listen 127.0.0.1:7391 --allow-host host.docker.internal

That is the only host-side command: the stack itself is started and stopped
through the start/stop tools, so alpha does not need to be running first.

--allow-host is what lets a loopback listener accept the name this container
dials; without it the request is refused with 403.
EOF
)"
exit 0
