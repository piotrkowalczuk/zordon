# Zordon

[![Go Reference](https://pkg.go.dev/badge/github.com/piotrkowalczuk/zordon.svg)](https://pkg.go.dev/github.com/piotrkowalczuk/zordon)
[![Go Report Card](https://goreportcard.com/badge/github.com/piotrkowalczuk/zordon)](https://goreportcard.com/report/github.com/piotrkowalczuk/zordon)
[![CI](https://github.com/piotrkowalczuk/zordon/actions/workflows/ci.yml/badge.svg)](https://github.com/piotrkowalczuk/zordon/actions/workflows/ci.yml)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE.md)

> **The local stack your inner loop runs against** — databases, brokers, proxies and services as fast, disposable **host processes**, declared once and supervised together. No containers.

Zordon is a supervisor for the local stack your code runs against — the databases, brokers, proxies and services that have to be alive for your code to be exercised for real.
It runs them as ordinary **host processes** instead of containers, declared once in an [`Alphasfile`](https://zordon.io/alphasfile/) and supervised together, so standing the stack up, duplicating it, and throwing it away are all cheap.

Three goals shape every decision.
**Low latency** — standing the stack up and re-running sit inside the loop, not around it; iteration shouldn't wait on infrastructure.
**Resourcefulness** — per-stack overhead stays small enough that many agents run side by side, each with a stack real enough to evaluate against rather than a wall of mocks.
**Guardrails** — the `Alphasfile` doubles as a contract, so the [MCP server](https://zordon.io/reference/mcp/) hands an agent only a narrow, declared surface: bring the stack up, inspect it, run a provision you named, and work through that interface instead of improvising against the host.

Why not containers?
Code is increasingly written in a loop — change something, run the real stack against it, read the result, go again, hundreds of times an hour.
Containers buy isolation by paying full cold-start and idle-overhead cost on *every* run: right for production, wasteful for a loop.
Zordon takes the other side, recovering isolation from per-run [workspaces](https://zordon.io/workspaces/) instead of images — so a second stack beside yours, or a tenth, is a non-event.

Declare the whole stack in one `Alphasfile`:

```hcl
service "go" "nats" {
  git {
    url = "github.com/nats-io/nats-server"
    tag = "v2.14.0"
  }

  vars = { client = net::pickport() }   # grab a free port, no clashes
  arguments { values = { main = { p = self.vars.client } } }
}
```

```sh
zordon start   # resolve, build, bring it up, stream logs until everything is READY
```

## Features

- **No containers, no daemon.** Your databases, brokers, proxies and services run as plain supervised host processes — nothing to image, mount or register.
- **Polyglot toolchains.** Go, Rust, Ruby, Node.js and Java (Maven/Gradle) services built straight from a git URL or local source; native packages (Redis, PostgreSQL, etcd, …) provisioned through [mise](https://mise.jdx.dev).
- **One manifest.** The whole stack — source, build, run, env, readiness, logs — declared in a single [`Alphasfile`](https://zordon.io/alphasfile/).
- **Dynamic configuration as a graph.** Values are functions, not strings: `net::pickport()` picks a free port, `fs::tmp()`/`fs::hash()` give per-run paths, and services reference each other (`service.go.caddy.vars.http`) with zero hardcoded wiring. → [docs](https://zordon.io/dynamic-config/)
- **Workspaces.** `zordon workspace create x` stands up a second, fully isolated copy of the entire stack — own ports, own dirs — an agent's sandbox beside yours, no port-mapping. → [docs](https://zordon.io/workspaces/)
- **Federation.** Alphasfiles chain by directory position: a project *sits below* shared infra instead of importing it. Move it in the tree and its environment recomputes. → [docs](https://zordon.io/federation/)
- **Readiness-aware bringup.** HTTP and exec readiness checks over a dependency graph; `zordon start` returns the moment every service is READY, then keeps the stack alive in the background.
- **Convergent re-runs.** Re-running costs only what actually changed — no blanket teardown and rebuild.
- **Provisions.** One-off setup (migrations, seeding, topic creation) declared per service, run on demand, with `zordon clean` for teardown.
- **Agent-friendly output.** `--agent` emits terse, structured logs; output is kept signal-dense because when a model reads your logs, tokens are the budget.
- **MCP server.** `zordon mcp` exposes every command — and every provision — as MCP tools, so an agent can drive the whole stack. → [docs](https://zordon.io/reference/mcp/)
- **Guardrails for agents.** The `Alphasfile` plus the MCP surface form a narrow, declared interface between agent and stack — it runs the *named* provisions you defined, not an open shell, so it can't improvise its way around your setup.
- **No runtime dependencies.** Three static Go binaries — `zordon`, `alpha`, `tommy` — and nothing else to install, image, or register.

## Documentation

Full docs: **<https://zordon.io>**
(source in [`docs/`](docs/), built with MkDocs Material).

- [Alphasfile](https://zordon.io/alphasfile/) — the manifest: services, source pointers, readiness, logs
- [Dynamic configuration](https://zordon.io/dynamic-config/) — the DAG, helpers, `self`, cross-service refs
- [Workspaces](https://zordon.io/workspaces/) — parallel, isolated copies of the whole stack
- [Federation](https://zordon.io/federation/) — chained Alphasfiles, shared infra, `zordon sudo`
- [MCP server](https://zordon.io/reference/mcp/) — drive zordon (and its provisions) from an agent over MCP
- [Install the Claude Code plugin](https://zordon.io/how-to/install-the-claude-code-plugin/) — zero-config setup via `/plugin install`

## Installation

Homebrew (macOS):

```sh
brew install piotrkowalczuk/tap/zordon
```

A release tarball (macOS and Linux, `arm64` and `amd64`) from
[Releases](https://github.com/piotrkowalczuk/zordon/releases):

```sh
tar -xzf zordon_<version>_<os>_<arch>.tar.gz -C ~/.local/bin zordon alpha tommy
```

Or from source, if you have a Go toolchain:

```sh
go install github.com/piotrkowalczuk/zordon/cmd/...@latest
```

Every variant installs the same three binaries — `zordon`, `alpha`, and
the `tommy` reaper. Keep them in one directory and make sure that
directory is on your `$PATH`: `zordon` finds `alpha` via `$PATH`, and
`alpha` finds `tommy` only as a sibling of its own executable. See
[Binaries and layout](https://zordon.io/reference/binaries/).

## Quick start

Create an `Alphasfile` (see [examples/simple](examples/simple/Alphasfile)), then:

```sh
zordon start              # spawn alpha, push config, stream bringup logs
zordon status             # what's running across the whole chain right now?
zordon stop               # ask alpha to shut its children down and exit
zordon clean              # run each provision's clean teardown (stack stopped)
zordon workspace create x  # a parallel, isolated copy of the stack
```

See the [docs](https://zordon.io) for everything else.

## Use with Claude (MCP)

`zordon mcp` runs an [MCP](https://modelcontextprotocol.io) server over stdio.
It exposes every zordon command — and every provision — as a tool, so an agent can drive your stack and run provisions on demand.

The fastest path is the Claude Code plugin — no manual config:

```sh
/plugin marketplace add piotrkowalczuk/zordon
/plugin install zordon@zordon
```

This registers the `zordon mcp` server and a skill nudging the agent toward it automatically; see the [install how-to](https://zordon.io/how-to/install-the-claude-code-plugin/).

For other MCP clients, or without the plugin:

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

See the [`zordon mcp` reference](https://zordon.io/reference/mcp/) and [how-to](https://zordon.io/how-to/run-a-provision-via-mcp/).

## License

[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl.txt),
see [LICENSE.md](LICENSE.md).
