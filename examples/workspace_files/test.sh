#!/usr/bin/env bash
# Claim: the top-level `workspace {}` block prepares a workspace DIRECTORY.
# `zordon workspace create` writes the declared files (create/merge/region)
# before anything is built or started, names the service checkout from the
# branch template, and never writes into src/. `workspace apply` re-runs it
# and, on an unchanged manifest, changes nothing.
EXDIR="$(cd "$(dirname "$0")" && pwd)"
cd "$EXDIR"
source ../_lib.sh

# A named workspace checks out HEAD, so the service sources have to be
# committed for the sparse path to materialize — a prerequisite, not a bug.
git -C "$ROOT" cat-file -e "HEAD:examples/workspace_files/src/app/main.go" 2>/dev/null \
	|| skip "examples/workspace_files/src/app not committed (a @HEAD workspace can't see it); commit the sources to run this"

build_bins
reset_state feature

zordon workspace create feature
trap 'cd "$EXDIR" && zordon stop --agent >/dev/null 2>&1 || true' EXIT

WS="$EXDIR/workspaces/feature"
assert_present "$WS/.workspace"

# create — whole files zordon owns. CLAUDE.md comes from a source template,
# so the interpolation must have happened rather than being copied verbatim.
assert_present "$WS/CLAUDE.md"
assert_contains "$(cat "$WS/CLAUDE.md")" "Workspace feature" "source template interpolated"
assert_present "$WS/.devcontainer/devcontainer.json"
dc="$(cat "$WS/.devcontainer/devcontainer.json")"
assert_contains "$dc" '"name": "zordon-feature"' "devcontainer names the workspace"
assert_contains "$dc" "zordon mcp --transport=http" "devcontainer starts the MCP server on the host"

# The derived port must be a real number, not an unresolved expression, and it
# must sit below Linux's ephemeral floor so the kernel cannot hand the same
# number to an unrelated socket.
port="$(printf '%s' "$dc" | sed -n 's/.*--listen 127\.0\.0\.1:\([0-9][0-9]*\) .*/\1/p')"
[ -n "$port" ] && [ "$port" -ge 20000 ] && [ "$port" -lt 32768 ] \
	&& pass "workspace port derived as $port" \
	|| fail "expected a derived port in 20000..32767, got '$port' from: $dc"

# The flags the devcontainer emits have to be ones zordon actually accepts —
# an invented flag would only surface when somebody ran the container.
"$ZORDON" mcp -h 2>&1 | grep -q -- "--transport" \
	&& pass "zordon mcp accepts --transport" \
	|| fail "zordon mcp has no --transport flag; the devcontainer command is wrong"

# merge and region wrote their own fragment into a file that did not exist.
assert_contains "$(cat "$WS/.claude/settings.json")" '"ZORDON_WORKSPACE": "feature"' "merge injected its key"
ignore="$(cat "$WS/.gitignore")"
assert_contains "$ignore" ">>> zordon: ignore" "region opened its marker"
assert_contains "$ignore" ".devcontainer/" "region wrote its body"

# The real claim of merge/region: zordon owns a FRAGMENT of a file somebody
# else owns. Replace both with hand-written content, re-apply, and what the
# user wrote must survive alongside what zordon injects.
printf '{"model":"opus","permissions":{"allow":["Bash"]}}' >"$WS/.claude/settings.json"
printf 'node_modules/\n' >"$WS/.gitignore"

zordon workspace apply --workspace=feature

settings="$(cat "$WS/.claude/settings.json")"
assert_contains "$settings" '"model": "opus"' "merge kept the hand-written key"
assert_contains "$settings" '"Bash"' "merge kept the hand-written nested value"
assert_contains "$settings" '"ZORDON_WORKSPACE": "feature"' "merge kept its own key"
ignore="$(cat "$WS/.gitignore")"
assert_contains "$ignore" "node_modules/" "region kept the hand-written line"
assert_contains "$ignore" ".devcontainer/" "region kept its body"

# The branch template names the checkout.
b="$(git -C "$WS/src/app" rev-parse --abbrev-ref HEAD)"
[ "$b" = "zordon/feature/app" ] \
	&& pass "app checked out on the templated branch ($b)" \
	|| fail "expected branch zordon/feature/app, got '$b'"

# Nothing was written into the service checkout: a generated file there would
# show up in that service's git status and risk being committed to its branch.
# Porcelain is the check rather than the absence of any particular name — a
# sparse cone also materializes the repo's OWN top-level files (including its
# CLAUDE.md), so presence proves nothing; being untracked would.
dirty="$(git -C "$WS/src/app" status --porcelain)"
[ -z "$dirty" ] \
	&& pass "service checkout left clean" \
	|| fail "workspace files leaked into src/app: $dirty"

# apply on an unchanged manifest is a no-op, byte for byte.
before="$(cat "$WS/CLAUDE.md" "$WS/.claude/settings.json" "$WS/.gitignore" | shasum | cut -d' ' -f1)"
zordon workspace apply --workspace=feature
after="$(cat "$WS/CLAUDE.md" "$WS/.claude/settings.json" "$WS/.gitignore" | shasum | cut -d' ' -f1)"
[ "$before" = "$after" ] \
	&& pass "workspace apply is idempotent" \
	|| fail "workspace apply changed the files on an unchanged manifest"

# The files exist before anything runs — that is the whole point — but the
# stack must still come up normally afterwards.
cd "$WS"
info "zordon start (workspace feature)"
zordon start --agent --timeout 120s
svcport="$(port_of "/app -addr")" || fail "no port for app"
body="$(http_get "http://127.0.0.1:$svcport/")" || fail "app not responding"
assert_contains "$body" "service=app" "app identifies itself"
