---
description: "Register the zordon plugin marketplace in Claude Code so the MCP server and its skill load without hand-editing `.mcp.json`."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/install-the-claude-code-plugin/">https://zordon.io/how-to/install-the-claude-code-plugin/</a></div>

# How to install the zordon Claude Code plugin

Make sure you have `zordon` on your `PATH` first — `brew install piotrkowalczuk/tap/zordon` on macOS, or a release tarball elsewhere.
See the [Install](../getting-started.md#install) step in Getting started.

Run the marketplace add command from any directory to register the zordon plugin marketplace:

```sh
/plugin marketplace add piotrkowalczuk/zordon
```

Then install the plugin:

```sh
/plugin install zordon@zordon
```

The plugin auto-registers the `zordon mcp` server and loads a skill that nudges the agent toward the MCP tools instead of raw `zordon` commands via Bash.
No `.mcp.json` or `CLAUDE.md` editing is needed.

To verify the server is connected, run `/mcp` from any project with an `Alphasfile`.
Ask the agent: *"what's the status of this project's stack?"* — it should reach for the `status` MCP tool, not Bash.

For details on what each tool does, see the [`zordon mcp` reference](../reference/mcp.md).
