---
description: "Serve zordon's MCP server over HTTP so an agent confined to a container or sandbox can manage the host-side dev stack through nothing but a URL."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/drive-a-host-stack-from-a-container/">https://zordon.io/how-to/drive-a-host-stack-from-a-container/</a></div>

# How to drive a host stack from a container

This recipe puts the agent in a box and leaves the stack on the host.
The agent gets an MCP URL and nothing else — no zordon binary, no supervisor socket, no shell on the host — so the only way it can touch the machine is through zordon's declared operations.

The stack does **not** need to be running first.
`zordon mcp` serves `start`, `stop` and `clean` whether or not alpha is up — that is precisely why it has to live outside the box, since nothing inside a container can start or kill a host process.
So the host side is one command, and the agent brings the stack up and tears it down itself.

For the full tool surface see [`zordon mcp`](../reference/mcp.md).

!!! warning "The endpoint is not authenticated yet"
    Anything that can reach the address can drive your stack.
    Bind it only where the sandbox you trust can reach it, and nowhere else.

## 1. Serve MCP over HTTP on the host

This is the only host-side command:

```sh
zordon mcp --transport=http --listen 127.0.0.1:7391 --allow-host host.docker.internal
```

It runs in the foreground and logs the URL it bound:

```
[…] mcp [INFO ] - serving over http on http://127.0.0.1:7391/mcp
[…] mcp [INFO ] - accepting Host header "host.docker.internal"
```

`--listen` takes `HOST:PORT` (also `ZORDON_LISTEN`) and defaults to `127.0.0.1:7391`.
Wildcard addresses — `0.0.0.0`, `::`, a bare `:7391` — are refused: name the interface the sandbox actually dials.
Add `--dir /path/to/project` if you start the server somewhere other than the project directory.

!!! tip "Or let the container start it for you"
    A dev container's [`initializeCommand`](https://containers.dev/implementors/json_reference/) runs *"on the host machine during initialization, including during container creation and on subsequent starts"* — the one hook in the whole setup that crosses the boundary.
    Every other lifecycle script, and every [Claude Code hook](https://code.claude.com/docs/en/hooks), runs inside the container and cannot reach the host.
    Point it at a script that starts this server when nothing is serving yet, and reopening the container is the only thing you ever do by hand.
    `examples/mcp_http/sandbox/.devcontainer/` does exactly that.

### Why `--allow-host`

A loopback listener accepts only a loopback `Host` header, which is what stops a web page open on your machine from driving the stack through `127.0.0.1`.
A container dials the host by *name*, so without `--allow-host` every request comes back `403 Forbidden: invalid Host header`.
The flag names the one hostname your box uses and leaves the protection in force for everything else.

Pass the name that appears in the client's URL, without the port.

## 2. Pick an address the sandbox can reach

Which address to bind depends on how the box reaches the host:

| Setup | Bind | `--allow-host` | Client URL |
| --- | --- | --- | --- |
| Docker Desktop (macOS, Windows) | `127.0.0.1:7391` | `host.docker.internal` | `http://host.docker.internal:7391/mcp` |
| Docker on Linux (bridge network) | the `docker0` address, e.g. `172.17.0.1:7391` | not needed | `http://172.17.0.1:7391/mcp` |
| Docker on Linux (`--network=host`) | `127.0.0.1:7391` | not needed | `http://127.0.0.1:7391/mcp` |
| A VM or a remote sandbox | the host's LAN address | not needed | `http://<lan-ip>:7391/mcp` |

`--allow-host` is only ever needed for a **loopback** bind; a non-loopback bind was already a deliberate choice and imposes no `Host` rule.

Mapping the name explicitly with `--add-host host.docker.internal:host-gateway` works under docker and podman alike, so one client config covers every runtime: bind loopback, pass `--allow-host host.docker.internal`, and the container always dials the same name.

## 3. Point the sandboxed client at the URL

Nothing is installed in the box for this — that is the advantage of the HTTP transport over stdio, where the client would have to launch a `zordon` binary that would need to exist inside the container.
Here the entire client side is a URL in a config file.

`examples/mcp_http/sandbox/` in this repository is a working dev container built on Claude Code's [dev container feature](https://code.claude.com/docs/en/devcontainer): `.mcp.json`, pre-approval, a preflight check, and a host-side `initializeCommand` that starts the server for you.
Copy its contents to your project root and **Reopen in Container**.

Inside the box, register it as an ordinary remote MCP server.
For Claude Code:

```sh
claude mcp add --transport http zordon http://host.docker.internal:7391/mcp
```

For a client that reads a JSON server config:

```json
{
  "mcpServers": {
    "zordon": {
      "type": "http",
      "url": "http://host.docker.internal:7391/mcp"
    }
  }
}
```

Nothing else goes into the image.
Because the URL is the only thing that identifies the stack, the same image and the same config can drive a different host by changing it.

!!! tip "Pre-approve the server, or every fresh container asks again"
    A project-scoped `.mcp.json` server needs approval on first use, and that approval is stored in the client's state — which lives *in the container* and dies with it.
    In Claude Code, commit a `.claude/settings.json` next to it so the box starts ready:

    ```json
    { "enabledMcpjsonServers": ["zordon"] }
    ```

    A `SessionStart` hook that probes the URL is worth adding too: an unreachable endpoint otherwise shows up as an agent with no zordon tools and no explanation.
    `examples/mcp_http/sandbox/` has both.

## 4. Verify from inside the box

List the tools: you should see one per zordon subcommand (`start`, `status`, `stop`, `get`, `plan`, …) plus one per provision in the resolved chain — the same set a stdio client gets, and the same set whether or not the stack is currently up.

Call `start`, then `status`.
`status` should name the services and their ports.
That round trip proves the whole path: the container sent control messages, the host executed them, and the process tree that came up is host-side — something the container could not have created itself.

Then run a provision on demand — see [How to run a provision via MCP](run-a-provision-via-mcp.md).
A provision that fails is reported as an error result and leaves the stack running.

## Shutting down

The agent can call `stop`, which tears the stack down but leaves the MCP server serving — so it can start the stack again later.

`Ctrl-C` (or `SIGTERM`) on the host stops the MCP server itself; in-flight requests drain first.
That does not stop the stack: services keep running under alpha until something calls `stop`.

## Troubleshooting

Almost every failure here is one of eight things, and most announce themselves.

| Symptom | Cause | Fix |
| --- | --- | --- |
| `403 Forbidden: invalid Host header "X"` | Loopback listener, container dialling by name. | `--allow-host X` — the error body names the exact value. |
| Connection refused; preflight reports the endpoint is down | No server on the host. | Start `zordon mcp --transport=http` there. |
| Agent has `start`/`status` but **no** `provision__*` tools | `--dir` points somewhere with no `Alphasfile`. | Check the server log for `provision tools unavailable`; fix `--dir`. The server still starts and answers, so this one is quiet — look for it. |
| `mcp --listen: … bind: address already in use` | Another server, or another stack, on that port. | Change `--listen` and the client URL together, or stop the other one. |
| `mcp --listen: refusing to bind the wildcard address` | `0.0.0.0`, `::`, or a bare `:7391`. | Name the interface the sandbox dials. |
| `mcp --listen: only valid with --transport=http` | `--transport=http` forgotten. | Add it. |
| A provision added to the `Alphasfile` never appears | The chain is resolved once, at startup. | Restart `zordon mcp`. |
| Every fresh container asks to approve the server | No `enabledMcpjsonServers`, and no persisted `~/.claude` volume. | Add both — see the tip above. |

The one worth internalising is the third: an endpoint answering `200` with no provision tools is a *working server pointed at the wrong directory*, not a broken one.

## See also

- `examples/mcp_http/sandbox/` — this recipe as a ready-to-open dev container, with the host-side server started from `initializeCommand`.
- `examples/mcp_http/` — the server side, driven end-to-end by `examples/mcp_http/example_test.go`.
- [`zordon mcp` reference](../reference/mcp.md) — transports, tool families, and the `OpInvoke` wire format.
- [Claude Code in a dev container](https://code.claude.com/docs/en/devcontainer) — installing the CLI, persisting `~/.claude` across rebuilds, and restricting network egress. Its MCP section is the counterpart to this page: it says to install a stdio server's binaries in your Dockerfile, which is exactly the cost an http server removes.
- [devcontainer.json reference](https://containers.dev/implementors/json_reference/) — `initializeCommand`, `runArgs`, `containerEnv`, and the rest of the spec.
