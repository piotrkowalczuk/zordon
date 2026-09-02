---
title: "Container per coding agent: what it isolates, and what it doesn't"
description: "Devcontainers and per-agent sandboxes give an agent a kernel boundary and charge full container cost per agent. Zordon isolates the stack instead — and the two compose."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/compare/devcontainers/">https://zordon.io/compare/devcontainers/</a></div>

# Container per coding agent: what it isolates, and what it doesn't

The standard answer to "how do I run several coding agents without them destroying each other" is a container per agent: a devcontainer, a VM-backed sandbox, one box per task.
It is a good answer, and it is answering a different question than zordon does.

- A **container per agent** isolates the *agent* — what it can read, write and reach.
- A **workspace** isolates the *stack* — which Postgres, which ports, which state dir it works against.

Those are orthogonal, which is why the interesting configuration is both.

## What a container per agent gives you

- A real security boundary. The agent cannot touch your home directory, your credentials, or your host network unless you let it.
- A pinned environment. The toolchain is in the image, so every agent starts identical.
- Blast-radius control. A destructive command stops at the container wall.

If your reason for isolating agents is *trust*, this is the mechanism and nothing on this page replaces it.

## What it costs

- **Full container price, per agent.** Image build or pull, cold start, and idle memory for each one — paid again every time you throw a box away, which under parallel agents is constantly.
- **A boundary your tools have to cross.** Editor, debugger and profiler are on the host; the process is not.
- **The stack question is still open.** Three devcontainers all pointing at one host Postgres is the exact shared-database problem you started with. Give each container its own database and you are now paying for N copies of the whole stack too.

## What zordon gives you instead

`zordon workspace create <name>` forks the whole stack for that agent: its own [`net::pickport()`](../dynamic-config.md) draw per service, its own `fs::state()` directory, its own per-service git worktrees on `zordon/<name>/<svc>`.
The marginal cost is a process tree, so the tenth workspace is as unremarkable as the second.

The agent gets a *declared* surface rather than a shell: [`zordon mcp`](../reference/mcp.md) exposes exactly the commands and the provisions you named in the `Alphasfile`.
That is a narrow interface, and a useful one — but it is a contract, not a wall.
An agent with shell access to the host is still an agent with shell access to the host.

## Side by side

| | Container per agent | zordon workspace |
|---|---|---|
| Isolates | the agent | the stack |
| Boundary | kernel-enforced | by construction, not enforced |
| Cost per extra agent | image + cold start + idle RAM | a process tree |
| Host tooling | across the boundary | attaches directly |
| Shared-database problem | still there, unless you duplicate the stack | solved by the fork |
| Best when | you do not trust the agent | you run many agents at once |

## Use both

This is the configuration the two are actually for:

1. The agent runs in its container or sandbox — the security boundary is real.
2. zordon runs the stack on the host, one workspace per agent.
3. The agent drives its workspace over the [HTTP MCP transport](../how-to/drive-a-host-stack-from-a-container.md), reaching it with nothing but a URL — no zordon binary and no control socket inside the sandbox.

`examples/mcp_http/sandbox/` in the repository is a ready-made containerised client config for exactly this.

Note the current limitation before you wire it up: the HTTP endpoint has [no authentication yet](../reference/mcp.md#http), so reachability is authorization — expose it only to the sandbox you mean to trust.

## Next

- [Run parallel coding agents against isolated stacks](../how-to/run-parallel-agents-with-workspaces.md)
- [Drive a host stack from a container](../how-to/drive-a-host-stack-from-a-container.md)
- [Workspaces](../workspaces.md) — the isolation model in full
