# Agent in a box, stack on the host

A dev container that runs Claude Code in isolation while the zordon stack stays on the host.

Three things make it work, and each one answers a problem that has no solution over stdio:

**Nothing zordon-related is installed in the container.**
A stdio MCP server would have to be — Claude Code's own [dev container guide](https://code.claude.com/docs/en/devcontainer) says as much: *"install any binaries that local stdio servers depend on in your Dockerfile"*.
An http server is a url in a config file, so this setup needs no custom Dockerfile at all: a stock base image plus the Claude Code dev container feature.

**Approvals survive a rebuild.**
Project-scoped MCP servers need approval, and that approval lives in `.claude.json` inside the container.
The volume mount plus `CLAUDE_CONFIG_DIR` keeps it across rebuilds; `enabledMcpjsonServers` covers the first run, before any volume exists.

**The agent owns the stack's lifecycle.**
`zordon mcp` serves `start`, `stop` and `clean` whether or not alpha is up.
That is exactly why it must run on the host: nothing inside a container can start or kill a host process.
`initializeCommand` — the one dev container hook that runs host-side, before the container exists — starts it for you.

## What is here

| File | Why |
| --- | --- |
| `.devcontainer/devcontainer.json` | Stock image + the Claude Code feature, the host alias, the config volume, and the host-side `initializeCommand`. |
| `.devcontainer/host-zordon-mcp.sh` | Starts `zordon mcp` on the host if it is not already serving. Idempotent, so a rebuild does not stack up servers. |
| `.mcp.json` | The zordon server as a project-scoped `http` entry. No `command`, so nothing is launched in the box. |
| `.claude/settings.json` | `enabledMcpjsonServers` pre-approves it, so the first session does not stop to ask. |
| `.claude/hooks/zordon-preflight.sh` | `SessionStart` check that turns "no zordon tools, no idea why" into the command to run on the host. |

## Use it

Open this directory in VS Code and run **Dev Containers: Reopen in Container**.

That is the whole procedure.
`initializeCommand` starts the host-side server before the container is built, so by the time the session opens the zordon tools are already there.

Ask the agent to bring the stack up and report on it.
It should call `start`, then `status`, and name the services and their ports — proving the whole path: the container sent control messages, the host executed them, and the process tree that came up is host-side, something the container could not have created.

To run it by hand instead, on the host:

```sh
zordon mcp --transport=http --listen 127.0.0.1:7391 --allow-host host.docker.internal
```

`--allow-host` is required.
A loopback listener otherwise accepts only a loopback `Host` header (DNS-rebinding protection) and this container dials by name, so every request would come back `403`.

## Adjusting it

- **Your own project** — copy this directory's contents to your project root. The default project dir is the folder holding `.devcontainer`, and zordon walks up from there for an `Alphasfile`, so it needs no edit. Override with `ZORDON_MCP_DIR` if you want another.
- **Port or hostname** — `ZORDON_MCP_PORT` and `ZORDON_MCP_ALLOW_HOST` steer the host script; keep the url in `.mcp.json` and `ZORDON_MCP_URL` in `devcontainer.json` in step with them.
- **Home directory** — `containerEnv` and `mounts` assume `remoteUser: vscode`. Change both paths together if you switch the base image.
- **Egress firewall** — if you adopt the reference container's `init-firewall.sh`, allowlist the host address, or the container will not reach this endpoint.

## Caveats

The endpoint is **not authenticated yet**: anything that can reach the address can drive the stack.
Bind it only where the box you trust can reach it.

Dev containers are a Docker-compatible format; Apple's `container` CLI is not a dev container runtime, so this bundle does not cover it.

See [the how-to](../../../docs/how-to/drive-a-host-stack-from-a-container.md) for the full explanation, and [the reference](../../../docs/reference/mcp.md) for every flag.
