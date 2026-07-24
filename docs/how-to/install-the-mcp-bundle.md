# How to install the MCP bundle

Use this to register zordon's MCP server in a client that is not Claude Code but supports MCP Bundles (`.mcpb`) — for example an editor agent that runs in your project.
Claude Code users should install the [plugin](install-the-claude-code-plugin.md) instead; it registers the same server with no download.

## Download the bundle for your platform

Grab the matching `.mcpb` from the [latest release](https://github.com/piotrkowalczuk/zordon/releases):

- `zordon_<version>_darwin_arm64.mcpb` — Apple Silicon
- `zordon_<version>_darwin_amd64.mcpb` — Intel Mac
- `zordon_<version>_linux_amd64.mcpb`
- `zordon_<version>_linux_arm64.mcpb`

Or grab `zordon_<version>_universal.mcpb` — one larger bundle that carries every platform and picks the right one at launch, if you would rather not match your platform by hand.

Each bundle is self-contained: it carries the `zordon`, `alpha`, and `tommy` binaries, so nothing else needs to be on your `$PATH`.

## Install it into your client

Follow your client's MCP-bundle install flow — usually opening or dragging the `.mcpb` file into it.
The bundle launches `zordon mcp` as a stdio server.

## Point it at your project

zordon's MCP tools operate on one `Alphasfile`-managed project.

The bundle asks for a **project directory** when you install it, and passes it to the server as `zordon mcp --dir`.
Set it to a directory that contains an `Alphasfile` (or sits under one), and `status`, `start`, and the rest act on that project — regardless of where your client launches the server.

Driving the server yourself instead of through the bundle? Pass the directory explicitly, or omit it to use the current one:

```sh
zordon mcp --dir /path/to/project
```

## Verify

Ask the agent for the stack status.
It should call the `status` tool and answer from your project, rather than shelling out to `zordon`.

For what each tool does, see the [`zordon mcp` reference](../reference/mcp.md).
