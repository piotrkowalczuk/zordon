#!/usr/bin/env bash
# Zordon plugin lifecycle hook dispatcher for Antigravity (and Claude Code).
# Delegates to sidecars/dashboard/server.py --hook <event>, keeping state
# shared between the lifecycle hooks and the Zordon sidecar dashboard pane.
set -u

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"
event="${1:-session-start}"

if command -v python3 >/dev/null 2>&1; then
	exec python3 "$root/sidecars/dashboard/server.py" --hook "$event"
fi

# Fallback if python3 is unavailable: use the shell adapter so the agent loop
# never blocks or errors.
# shellcheck source=./adapter.sh
source "$here/adapter.sh"
hook_read_stdin
case "$event" in
	pre-tool) hook_pre_allow ;;
	post-tool) hook_post_ok ;;
	*) hook_session_message "" ;;
esac
