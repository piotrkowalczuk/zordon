---
description: "Reference for the MCP server zordon serves over stdio or HTTP — transports, discoverability, and the tools generated from declared provisions."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/reference/mcp/">https://zordon.io/reference/mcp/</a></div>

# `zordon mcp`

`zordon mcp` serves a [Model Context Protocol](https://modelcontextprotocol.io) server.
It is an additional surface on the same CLI, not a replacement: zordon stays a CLI, and the MCP server exposes that CLI to agents.
Run it from a project directory (one containing an `Alphasfile`, or nested under one); it resolves the same federation chain the CLI would.

## Transports

`--transport` selects how clients reach the server.
The tool set is identical either way — the transport changes only who may connect and from where.

| Flag | Env | Default |
| --- | --- | --- |
| `--transport stdio\|http` | `ZORDON_TRANSPORT` | `stdio` |
| `--listen HOST:PORT` | `ZORDON_LISTEN` | `127.0.0.1:7391` (only with `--transport=http`) |
| `--allow-host HOST` (repeatable) | `ZORDON_ALLOW_HOST` | none (only with `--transport=http`) |

### stdio (default)

The server speaks newline-delimited JSON-RPC 2.0 over stdin/stdout (the standard local MCP transport).
Diagnostics go to stderr; stdout carries only the protocol.
Launch it the way an MCP client launches any local server — as a subprocess whose stdin/stdout it owns.

Passing `--listen` with `--transport=stdio` is an error rather than a silently ignored flag.

### http

`zordon mcp --transport=http` serves the [streamable-HTTP transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports) — the standard remote-MCP shape — at `http://HOST:PORT/mcp`.
Any other path returns 404.
The process runs in the foreground until interrupted; `SIGINT`/`SIGTERM` drain in-flight requests before it exits.

This is what lets an MCP client that is *isolated from the host* drive the stack.
A container or OS sandbox cannot spawn host processes, stdio does not cross the boundary, and a bind-mounted unix socket is inert inside a VM-backed container — so the stdio model requires the agent to run on the host with full host access.
Over HTTP the agent needs nothing but the URL: no zordon binary and no alpha socket inside the sandbox.
The supervisor, the service processes, and the whole stack stay host-side, and the agent only sends control messages that the host executes.
Because the URL selects the stack, one client image or config can target different hosts by changing it.

Once bound, the server logs the resolved URL to stderr:

```
[…] mcp [INFO ] - serving over http on http://127.0.0.1:7391/mcp
```

Port `0` binds an ephemeral port, which that line then reports.

!!! warning "The HTTP endpoint has no authentication yet"
    Anything that can reach the address can drive the host stack — start and stop services, run provisions, execute every declared operation.
    Authentication is planned; until it lands, treat reachability as authorization and expose the endpoint only to the sandbox you intend to trust.

Two guardrails follow from that:

- **Wildcard binds are refused.** `0.0.0.0`, `::`, and a bare `:PORT` are rejected; name the interface the client reaches you on. Binding a non-loopback address is allowed and logs a warning.
- **DNS-rebinding protection is on.** On a loopback bind, a request whose `Host` header is not itself loopback is rejected with `403 Forbidden: invalid Host header`. This is what stops a web page open on this machine from being talked into driving your stack through `127.0.0.1`.

### `--allow-host`

A container usually dials the host by *name*, not by loopback — `host.docker.internal` on Docker Desktop — so the rule above would reject it.
`--allow-host` widens the accepted `Host` headers without giving up the protection for everything else:

```sh
zordon mcp --transport=http --listen 127.0.0.1:7391 --allow-host host.docker.internal
```

The flag is repeatable, takes a hostname without a port, and only applies to a loopback `--listen` (a non-loopback bind was already a deliberate act and imposes no `Host` rule).
Anything not loopback and not named is still refused, and the 403 body names the flag that would admit it.
Passing it with `--transport=stdio` is an error.

See [How to drive a host stack from a container](../how-to/drive-a-host-stack-from-a-container.md) for the full recipe, and `examples/mcp_http/sandbox/` for a ready-made containerized client config.

## Discoverability

The agent decides when to use these tools from three signals (it is the model's judgement — there is no hard routing):

- **The Claude Code plugin** — installing it (see [how-to](../how-to/install-the-claude-code-plugin.md)) registers the server and a skill automatically; the manual signals below are what it replaces.
- **Server `instructions`** — at `initialize` the server returns an `instructions` string (MCP's server-purpose field) that the client injects into the agent's context: what zordon is and when to reach for it, scoped so it is ignored where there is no `Alphasfile`.
- **Per-tool descriptions** — each tool carries a description (a command's help; a provision's resolved `cmd`, flags, and the no-kill-alpha note).
- **Project context** — for Claude Code, a line in `CLAUDE.md`/`AGENTS.md` ("this project's stack is managed by zordon — use the `zordon` MCP tools") is the strongest nudge.

## Tool families

The server registers two kinds of tools. Call `tools/list` to discover them.

### Command tools

One tool per zordon subcommand, generated by introspecting the same command tree the CLI uses — so the tool list can never drift from the CLI.
Every subcommand is exposed except `mcp` itself.

| Field | Value |
| --- | --- |
| Name | the subcommand name (`start`, `status`, `stop`, `clean`, `sudo`, `workspace`, `plan`, `get`) |
| Input | `{ "args": ["<flag-or-arg>", ...] }` — flags and positional arguments, exactly as on the CLI; pass `["-h"]` for the full flag list |
| Output | the command's combined stdout+stderr, captured into the tool result |

The command runs in-process against capture buffers; its output never reaches the JSON-RPC stdout channel.
The result's `isError` is set when the command exits non-zero.

### Provision tools

One tool per provision found in the resolved chain (across every federation level).

| Field | Value |
| --- | --- |
| Name | `provision__<toolchain>_<service>__<step>`, sanitized to `^[A-Za-z0-9_-]{1,64}$` and de-duplicated across levels |
| Description | the provision's `description` attribute (if set in the Alphasfile), followed by the resolved `cmd`, env keys, and the no-kill-alpha note |
| Input | `{ "env": { "KEY": "value", ... } }` — overrides overlaid on the provision's own env (highest precedence) |
| Output | the provision's streamed shell output; `isError` is set on failure |

Give a provision a human-readable summary with the optional `description` attribute; it leads the generated tool description.

### Typed arguments

A provision declares typed inputs with `argument "<name>" { ... }` blocks. Each becomes a named, typed field on the MCP tool's input schema, so the agent sees exactly what to pass:

```hcl
provision "seed-data" {
  description = "Write the seed marker file from the `key` argument"
  after       = never

  argument "key" {
    type        = "string"   # string | number | bool (default string)
    required    = true
    description = "value written to the seed marker file"
  }

  cmd = "echo ${self.runtime.provision.seed-data.arguments.key} > ${self.vars.seed}"
}
```

| `argument` field | Meaning |
| --- | --- |
| `type` | `string`, `number`, or `bool` (default `string`); drives the schema field type and value coercion |
| `required` | listed in the schema's `required`; a missing value at invoke is an error |
| `default` | a concrete value (resolved at eval) used when the caller omits the argument |
| `description` | the schema field's description |

Reference an argument in `check`/`cmd`/`verify` by its full path `self.runtime.provision.<name>.arguments.<arg>`.
At configure each such reference resolves to a placeholder; alpha substitutes the supplied value at invoke.
Rules (enforced at configure): a provision with arguments **must be latent** (`after = never`); it may **not** be a `cmd`-ref target; and it may reference only **its own** arguments.
The generic `env` field stays available on every provision tool for undeclared overrides.

Invoking a provision tool dials that level's running alpha and runs the provision inside it (see [OpInvoke](#opinvoke)).
If no alpha is running for that level, the tool returns an error result directing you to run `zordon start`.

Provisions are listed whether or not alpha is running (so an agent can discover them, then start, then invoke), but invoking one requires a running alpha.

## OpInvoke

A provision tool call maps to a single control-socket request:

```json
{"op": "invoke", "invoke": {"provision": "service.<tc>.<svc>.runtime.provision.<name>", "args": {"key": "value"}, "env": {"KEY": "value"}}}
```

`provision` is the canonical provision id (the same identity alpha keys provisions by).
`args` are validated against the provision's declared `argument` blocks and substituted into the snippets' placeholders; `env` is a free-form overlay.
alpha then runs the provision via its normal `runProvision` path — reusing the parent service's env and working directory, the toolchain, and the process-group / shutdown handling.
The response is the same streamed `Event` sequence as `configure`: `log` events for shell output, terminated by `done` (success) or `error` (failure).

### Invariant: a failed invoke never kills alpha

An on-demand invoke runs with `failfast = false`.
A provision that exits non-zero reaches its `failure` terminal and the tool reports an error, but alpha keeps running — unlike a bringup-time provision under the default `--failfast`, which shuts the stack down.
An invoke also runs immediately (no `after` wait): the parent service is assumed already up because alpha is running.

## Requirements and scope

- The server resolves the chain from its working directory — launch it inside the project, or point it at one with `zordon mcp --dir DIR` (also `ZORDON_DIR`).
- The chain is resolved once, at startup. A long-running server does not pick up later `Alphasfile` edits; restart it to refresh the provision tools.
- Provision invocation requires a running alpha for that level; the server does not auto-start one.
- Re-invoking an auto-run provision is allowed (re-run a migration or smoke test on demand); idempotent provisions short-circuit via their `check`.

## Related

- [How to drive a host stack from a container](../how-to/drive-a-host-stack-from-a-container.md) — the HTTP transport end to end.
- [How to run a provision via MCP](../how-to/run-a-provision-via-mcp.md) — a step-by-step recipe.
- [Lifecycle](../lifecycle.md) — provision states (`scheduled` → `running` → `success`/`failure`) and `after` barriers.
- [Alphasfile](../alphasfile.md) — declaring provisions, including latent (`after = never`) ones.
