---
description: "Declare the CLAUDE.md, dev container and settings a workspace needs, so an agent can be dropped into it and reach the stack."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/prepare-a-workspace-for-an-agent/">https://zordon.io/how-to/prepare-a-workspace-for-an-agent/</a></div>

# Prepare a workspace for an agent

Goal: `zordon workspace create feature` produces a directory you can drop a containerized agent into, with the stack reachable from inside the container.

This assumes you already have an Alphasfile that starts.
See [Workspaces](../workspaces.md) for the model.

## 1. Declare the files

Add one top-level `workspace {}` block.

```hcl
workspace {
  file "claude" {
    path = "CLAUDE.md"
    create { source = ".zordon/templates/CLAUDE.md" }
  }

  file "devcontainer" {
    path = ".devcontainer/devcontainer.json"
    create {
      body = enc::json({
        name  = "zordon-${workspace.name}"
        image = "mcr.microsoft.com/devcontainers/go:1"
        features = {
          "ghcr.io/anthropics/devcontainer-features/claude-code:1.0" = {}
        }
        initializeCommand = "zordon mcp --http --port ${workspace.port} --allow-host"
        containerEnv = {
          ZORDON_WORKSPACE = workspace.name
        }
      })
    }
  }

  file "mcp" {
    path = ".mcp.json"
    merge {
      data = {
        mcpServers = {
          zordon = {
            type = "http"
            url  = "http://host.docker.internal:${workspace.port}"
          }
        }
      }
    }
  }
}
```

## 2. Understand which hook runs where

This is the part that decides the whole shape.

`initializeCommand` is a dev container hook, not an agent one, and it is the **only** hook that runs on the **host** before the container exists.
That makes it the one place that can start a server the container will later talk to.

The agent's own hooks — `SessionStart` and the rest, configured in `.claude/settings.json` — run wherever the agent runs, which is *inside* the container.
They cannot reach the host, so they cannot start the MCP server.

So the two files carry different jobs: `devcontainer.json` starts the server on the host, `.claude/settings.json` configures what happens in the container.

## 3. Let the port be stable

The container needs an address for a server that does not exist yet when the file is written.
`net::pickport()` cannot do this — it draws a fresh port on every evaluation, so the file and the server would disagree.

`workspace.port` is derived from the workspace's hash instead, so it is the same on every run of that workspace.
That is what lets a file written at `workspace create` name a server started later.

## 4. Create the workspace

```sh
zordon workspace create feature
cd workspaces/feature
```

The files are already there.
Order matters and it is deliberate: `workspace create` writes them, *then* checks out the services — so an agent opening the directory finds its configuration before any stack is running.

## 5. Apply changes to an existing workspace

Editing the Alphasfile does not reach workspaces that already exist.

```sh
zordon workspace apply --workspace=feature
```

Running it twice changes nothing: `create` rewrites only what differs, `merge` is an RFC 7386 patch, and `region` replaces its marked span rather than appending.

For the project root, which is never "created":

```sh
zordon workspace apply
```

Do that one deliberately — the project root is your real repository, and a `merge` there modifies a file git is tracking.

## What you cannot put in these files

Anything that only exists once the stack runs: a service's port, `self.*`, `service.<tc>.<svc>.*`.
They are rejected with an error rather than resolved into something wrong.

Two consequences worth planning around:

A generated `.mcp.json` is read by the agent **at session start**.
In the dev container flow above that is fine, because the container starts after the file is written.
Writing one into a session that is already running will not be noticed until it restarts.

Values that genuinely depend on a running service belong in that service's own `file {}` block, which alpha materializes at `zordon start` — or are read live through `zordon get` and the MCP tools, which do refresh.
