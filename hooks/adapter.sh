#!/usr/bin/env bash
# Shared hook adapter for Claude Code and Antigravity.
#
# Both agents pass a JSON payload on stdin to lifecycle hooks, but they differ
# in field names and output contracts:
#
#   Event          Claude Code (stdin -> stdout/exit)        Antigravity (stdin -> stdout/exit)
#   -------------  ----------------------------------------  ---------------------------------------------------
#   SessionStart   .cwd -> plain text on stdout              .workspacePaths[0] -> {"injectSteps":[...]} JSON
#   PreToolUse     .tool_input.{command,file_path}           .toolCall.args.{CommandLine,Path}
#                  allow: exit 0                             allow: {"decision":"allow"} on stdout
#                  deny:  stderr + exit 2                    deny:  {"decision":"deny","reason":...} on stdout
#   PostToolUse    .tool_input.{command,file_path}           .toolCall.args.{CommandLine,Path}
#                  ok:    exit 0                             ok:    {} on stdout
#                  fail:  stderr + exit 2                    fail:  stderr + exit 2

HOOK_INPUT=""
HOOK_ENGINE="claude"

hook_read_stdin() {
	HOOK_INPUT="$(cat)"
	if printf '%s' "$HOOK_INPUT" | jq -e 'has("conversationId") or has("toolCall") or has("workspacePaths")' >/dev/null 2>&1; then
		HOOK_ENGINE="antigravity"
	else
		HOOK_ENGINE="claude"
	fi
}

hook_cwd() {
	printf '%s' "$HOOK_INPUT" | jq -r '.cwd // .workspacePaths[0] // empty' 2>/dev/null
}

hook_tool_name() {
	printf '%s' "$HOOK_INPUT" | jq -r '.tool_name // .toolCall.name // empty' 2>/dev/null
}

hook_command() {
	printf '%s' "$HOOK_INPUT" | jq -r '.tool_input.command // .toolCall.args.CommandLine // empty' 2>/dev/null
}

hook_file_path() {
	printf '%s' "$HOOK_INPUT" | jq -r '.tool_input.file_path // .toolCall.args.Path // empty' 2>/dev/null
}

hook_error() {
	printf '%s' "$HOOK_INPUT" | jq -r '.error // empty' 2>/dev/null
}

hook_pre_allow() {
	if [ "$HOOK_ENGINE" = "antigravity" ]; then
		printf '{"decision":"allow"}\n'
	fi
	exit 0
}

hook_pre_deny() {
	local reason="${1:-blocked by hook}"
	printf '\n%s\n' "$reason" >&2
	if [ "$HOOK_ENGINE" = "antigravity" ]; then
		jq -cn --arg r "$reason" '{decision: "deny", reason: $r}'
		exit 0
	fi
	exit 2
}

hook_post_ok() {
	if [ "$HOOK_ENGINE" = "antigravity" ]; then
		printf '{}\n'
	fi
	exit 0
}

hook_post_fail() {
	local reason="${1:-post-tool hook failed}"
	printf '\n%s\n' "$reason" >&2
	exit 2
}

hook_session_message() {
	local msg="${1:-}"
	if [ -z "$msg" ]; then
		if [ "$HOOK_ENGINE" = "antigravity" ]; then
			printf '{}\n'
		fi
		exit 0
	fi
	if [ "$HOOK_ENGINE" = "antigravity" ]; then
		jq -cn --arg m "$msg" '{injectSteps: [{systemMessage: {systemMessage: $m}}]}'
	else
		printf '%s\n' "$msg"
	fi
	exit 0
}
