---
title: "Run parallel coding agents against isolated stacks"
description: "Give every parallel agent its own workspace — its own ports, its own state dir, its own service checkouts — so several agents work the same project at once without colliding."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/run-parallel-agents-with-workspaces/">https://zordon.io/how-to/run-parallel-agents-with-workspaces/</a></div>

# Run parallel coding agents against isolated stacks

You are running more than one coding agent on the same project, each in its own checkout.
The code is isolated; the stack is not.
Every agent still dials the same Postgres, the same Redis and the same dev server port, and whichever one migrates or seeds first decides what the others see.

A **workspace** is the other half of that isolation: a second copy of the whole stack over the *same* `Alphasfile`, with its own ports, its own state dir and its own service checkouts.

## 1. Create one workspace per agent

```sh
zordon workspace create alice
zordon workspace create bob
```

Each call materializes `workspaces/<name>/` with a `.workspace` marker and a per-service `git worktree` on branch `zordon/<name>/<svc>`.
Services the agent only *runs* — a `pkg` database, an unpicked `git` source — are not checked out; there is nothing there to edit.

To let an agent edit one service and run the rest as-is, name it:

```sh
zordon workspace create alice api      # only `api` gets an editable checkout
zordon workspace create alice api@v2   # …pinned to revision v2
```

## 2. Start the stack inside the workspace

```sh
cd workspaces/alice
zordon start --agent
```

zordon walks up from the workspace dir, adopts the project `Alphasfile` as the leaf, and runs it as workspace `alice`.
The invocation dir differs, so `fs::hash()` differs — which means a distinct state dir, a distinct control socket, and a **fresh `net::pickport()` draw for every service**.
There is no port map to maintain, no project name to pass, and nothing to reserve up front.

`--agent` keeps the output terse and structured, which is what you want when a model is the one reading it.

Check what each workspace actually got:

```sh
zordon status
```

## 3. Point the agent at its own workspace

`zordon mcp` resolves the stack from its working directory, exactly like the CLI does.
Start it with the workspace as the working directory and the agent drives that stack and no other:

```sh
cd workspaces/alice
zordon mcp
```

If the agent runs inside a container or an OS sandbox, serve the same tools over HTTP instead — see [Drive a host stack from a container](drive-a-host-stack-from-a-container.md).

## 4. Keep the shared things out of the shared level

A workspace forks the **leaf** level only; federation parents above the project are reused as they are.
So anything the agents must not share — Postgres, Redis, the broker — belongs in the project `Alphasfile`, not in a parent one:

```hcl
service "pkg" "postgres" {
  package = "asdf:mise-plugins/mise-postgres@16.4"

  vars = { port = net::pickport() }   # a fresh port per workspace

  runtime {
    cmd = ["postgres", "-D", "${fs::state()}/pgdata", "-p", "${self.vars.port}"]
  }

  readiness { tcp { port = self.vars.port } }
}
```

A package that needs one-time setup (`initdb` here) gates the daemon on a provision — see [Run a native package as a supervised service](../services/pkg.md#one-time-init-eg-postgres-initdb).

The same rule applies to anything on disk: derive paths from `fs::state()`, which is already per-workspace, rather than from a fixed path.

## 5. Throw it away

```sh
zordon workspace list                 # from the project root
cd workspaces/alice && zordon stop
cd ../.. && zordon workspace rm alice
```

`rm` detaches the per-service worktrees and removes their trees.
zordon never resets or deletes a checkout it does not recognize as its own — a checkout you moved to another branch is reused with a warning — but `workspace rm` does remove what it created, so commit or copy out an agent's work before dropping the workspace.

## Notes

- **The branch is the interlock.** If `zordon/<name>/<svc>` is already checked out somewhere else, you get a clear error naming it, not a raw git failure.
- **A big monorepo.** Use `workspace { sparse = [...] }` so each checkout materializes only the subtree that service needs — see [Partial checkout](../workspaces.md#partial-checkout-sparse).
- **The model behind this** is in [Workspaces](../workspaces.md); the runnable version is [`examples/workspace`](https://github.com/piotrkowalczuk/zordon/tree/main/examples/workspace).
