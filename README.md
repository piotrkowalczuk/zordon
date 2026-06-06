# Zordon

Zordon runs the supporting stack for local agentic workflows — the
databases, queues, brokers, and toy services an agent needs to actually
exercise your code while it works.

The constraint is resourcefulness. An agent loop spins these dependencies
up and tears them down constantly; every second of cold start and every
megabyte of idle overhead is paid back many times a day. Containers solve
isolation by paying that cost upfront — fine for production, wasteful here.
Zordon runs the same services as plain host processes, supervised
together, declared once in an `Alphasfile`.

## Documentation

Full docs: **<https://piotrkowalczuk.github.io/zordon/>**
(source in [`docs/`](docs/), built with MkDocs Material).

- [Alphasfile](docs/alphasfile.md) — the manifest: services, source pointers, readiness, logs
- [Dynamic configuration](docs/dynamic-config.md) — the DAG, helpers, `self`, cross-service refs
- [Worktrees](docs/worktrees.md) — parallel, isolated copies of the whole stack
- [Federation](docs/federation.md) — chained Alphasfiles, shared infra, `zordon sudo`
- [MCP server](docs/reference/mcp.md) — drive zordon (and its provisions) from an agent over MCP

## Installation

```sh
go install github.com/piotrkowalczuk/zordon/cmd/...@latest
```

Installs `zordon` and `alpha` into your `$GOBIN` (or `$GOPATH/bin`) —
make sure that directory is on your `$PATH`.

## Quick start

Create an `Alphasfile` (see [examples/simple](examples/simple/Alphasfile)), then:

```sh
zordon start              # spawn alpha, push config, stream bringup logs
zordon status             # what's running across the whole chain right now?
zordon stop               # ask alpha to shut its children down and exit
zordon worktree create x  # a parallel, isolated copy of the stack
```

See the [docs](https://piotrkowalczuk.github.io/zordon/) for everything else.

## Use with Claude (MCP)

`zordon mcp` runs an [MCP](https://modelcontextprotocol.io) server over stdio.
It exposes every zordon command — and every provision — as a tool, so an agent can drive your stack and run provisions on demand.

The MCP **client launches the server**; you don't run `zordon mcp` yourself.
With Claude Code, register it from your project directory:

```sh
claude mcp add zordon -- zordon mcp
```

Or add it to your client's MCP config (e.g. `.mcp.json`):

```json
{ "mcpServers": { "zordon": { "command": "zordon", "args": ["mcp"] } } }
```

The server resolves the chain from its working directory, so launch the client from the project tree (or pass `-e ZORDON_HOME=…`).
Provisions run inside the live `alpha`, so `zordon start` first — or let the agent call the `start` tool.

The server advertises its purpose to the agent via MCP `instructions` (when to reach for these tools), so it should pick them up on its own.
To nudge it harder in a zordon-managed repo, add a line to your `CLAUDE.md` (or `AGENTS.md`): *"this project's local stack is managed by zordon — use the `zordon` MCP tools to bring it up, inspect it, and run provisions."*

See the [`zordon mcp` reference](docs/reference/mcp.md) and [how-to](docs/how-to/run-a-provision-via-mcp.md).

## License

[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl.txt),
see [LICENSE.md](LICENSE.md).
