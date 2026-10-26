---
description: "Install the zordon plugin in Antigravity so the MCP server, lifecycle hooks, skill and sidecar dashboard load automatically."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/install-the-antigravity-plugin/">https://zordon.io/how-to/install-the-antigravity-plugin/</a></div>

# How to install the zordon Antigravity plugin

Make sure you have `zordon` on your `PATH` first — `brew install piotrkowalczuk/tap/zordon` on macOS, or a release tarball elsewhere.
See the [Install](../getting-started.md#install) step in Getting started.

Install the plugin from a local checkout of the repository or via the Antigravity CLI:

```sh
agy plugin install /path/to/zordon
```

Or inside an interactive `agy` session, run `/plugin` and choose **+ Install from local directory...**.

The plugin bundles four components:

- `mcp_config.json` — auto-registers the `zordon mcp` server over stdio.
- `skills/zordon/SKILL.md` — teaches the agent when to use workspaces and the MCP tools instead of raw shell commands.
- `hooks.json` — runs a `SessionStart` hook that injects the current stack summary when an `Alphasfile` is present and records MCP tool calls for the dashboard.
- `sidecars/dashboard/sidecar.json` — serves a live **Zordon** auxiliary pane showing the workspace checkouts, runtime service status, recent MCP call logs, and per-service git diffs (`src/<app>`).

Restart your Antigravity session after installing so the newly registered plugin directory and sidecar are discovered.

To verify the server is connected, ask the agent: *"what's the status of this project's stack?"* — it should reach for the `status` MCP tool, not Bash.

For details on what each tool does, see the [`zordon mcp` reference](../reference/mcp.md).
