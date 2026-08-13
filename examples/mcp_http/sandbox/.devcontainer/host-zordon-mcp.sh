#!/usr/bin/env sh
# devcontainer initializeCommand: runs on the HOST, before the container is
# created. That makes it the one place in this whole setup that can start a
# host-side process — which is what `zordon mcp` has to be, since it is the
# thing that starts and kills alpha.
#
# Idempotent: if something already answers on the port, leave it alone.
set -eu

PORT="${ZORDON_MCP_PORT:-7391}"
LISTEN="127.0.0.1:$PORT"
ALLOW_HOST="${ZORDON_MCP_ALLOW_HOST:-host.docker.internal}"
# The project whose Alphasfile is served: the workspace folder holding this
# .devcontainer. zordon walks up from there for an Alphasfile, so this is right
# both for a project that owns its own and for this example, whose Alphasfile
# sits one level above the sandbox.
DIR="${ZORDON_MCP_DIR:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}"
LOG="${ZORDON_MCP_LOG:-${TMPDIR:-/tmp}/zordon-mcp-$PORT.log}"

probe() {
	curl -fs -o /dev/null --max-time 2 \
		-X POST "http://$LISTEN/mcp" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
}

if probe; then
	echo "zordon mcp already serving on http://$LISTEN/mcp"
	exit 0
fi

if ! command -v zordon >/dev/null 2>&1; then
	echo "zordon not found on PATH — install it on the host, then rebuild the container." >&2
	exit 1
fi

echo "starting zordon mcp on http://$LISTEN/mcp (dir: $DIR, log: $LOG)"
# Detached: initializeCommand blocks until this script exits, and the server is
# meant to outlive it.
nohup zordon mcp --dir "$DIR" --transport=http --listen "$LISTEN" --allow-host "$ALLOW_HOST" \
	>"$LOG" 2>&1 &

# Give it a moment to bind, so a misconfiguration surfaces here rather than as
# a container that comes up with no zordon tools.
i=0
while [ "$i" -lt 20 ]; do
	if probe; then
		echo "zordon mcp is up"
		exit 0
	fi
	i=$((i + 1))
	sleep 0.25
done

echo "zordon mcp did not come up within 5s; see $LOG" >&2
exit 1
