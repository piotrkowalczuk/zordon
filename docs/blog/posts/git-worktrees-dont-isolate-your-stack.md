---
title: "Git worktrees isolate your code. They don't isolate your stack."
description: "Every guide to running parallel AI coding agents ends at git worktrees. Worktrees isolate files — not ports, not Postgres, not state. Here is what breaks and what closes the gap."
slug: git-worktrees-dont-isolate-your-stack
date: 2026-09-01
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/blog/git-worktrees-dont-isolate-your-stack/">https://zordon.io/blog/git-worktrees-dont-isolate-your-stack/</a></div>

# Git worktrees isolate your code. They don't isolate your stack.

Every guide to running several coding agents at once lands on the same advice, and the advice is right: give each agent its own `git worktree`.
Three agents, three directories, three branches, one `.git`.
No more agents overwriting each other's files mid-edit.

Then you start the second agent and its dev server dies on `address already in use`.

<!-- more -->

## What a worktree actually isolates

A worktree is a second checkout backed by the same object database.
It gives each agent its own working tree, its own index, and its own branch.

That is the entire list.

A worktree has no opinion about anything that is not a file in the repo:

- **Listening ports.** Three worktrees, one `:3000`, one `:5432`, one `:6379`.
- **Databases.** `postgresql://localhost/app_dev` resolves to the same database from every worktree, because it is the same string on the same host.
- **State on disk.** Upload dirs, SQLite files, caches, `/tmp` scratch space — all outside the repo, all shared.
- **Anything a daemon owns.** A running Docker container, a system Redis, a `brew services` Postgres. None of them know worktrees exist.

So the code is forked and the stack is not, which produces a specific and nasty failure mode.

## The failure mode is not the crash

`address already in use` is the *good* case.
It is loud, it happens at startup, and you fix it in a minute.

The expensive case is quiet.
Agent B runs a migration; agent A's test suite, mid-run, starts failing against a schema it never asked for.
Agent C seeds fixtures; agent A's assertion on `SELECT count(*)` flips.
Agent A reads the failure, concludes its own last edit broke something, and starts *fixing code that was never broken*.

That is the real cost of a shared stack under parallel agents.
An agent's loop is only as good as the signal it reads, and a shared database turns the signal into noise it has no way to attribute.
You do not lose a few minutes to a port collision — you lose a whole loop to a wrong conclusion, and you pay again when you review the diff it produced.

## The three usual answers

**1. Hand-assigned ports.**
A `.env` per worktree, `PORT=3001`, `PORT=3002`.
Cheapest thing that works, and for one service it is genuinely fine.
It stops being fine the moment services have to find *each other*: now every worktree needs a consistent set of N ports, the numbers are static so two agents on the same number still collide, and every new service multiplies the bookkeeping.
Nobody maintains this table for long.

**2. `docker compose -p <worktree>` per agent.**
This one is real isolation: separate networks, separate volumes, separate containers.
It also charges you the full container price on every stack you stand up, and the whole point of running agents in parallel is that you stand up and tear down stacks constantly.
Cold start and idle RAM look harmless once; multiplied across three stacks and a loop that runs hundreds of times an hour, they *are* the experience.
Published ports still collide too, unless you stop pinning host ports — at which point you are back to discovering which port you got.

**3. A container or VM per agent.**
The strongest isolation available, and the right answer when the threat model needs it — an agent that must not be able to touch the host.
The cost is that the agent is now inside the box while your editor, debugger and profiler are outside it, and everything has to cross that boundary.

Each of these is a reasonable trade.
What they have in common is that duplicating a *stack* is expensive, so you ration it — and rationing is exactly what breaks when the number of agents goes up.

## The property that actually matters

If a second copy of the whole stack were as cheap as a second checkout, none of this would be a decision.
You would give every agent its own, the way you already give every agent its own worktree, and stop thinking about it.

That is what [zordon](https://zordon.io) is built around.
Services run as ordinary supervised host processes rather than containers, so a second stack costs a process tree, not an image and a cold start.
Isolation comes from the run, not from a sandbox.

The mechanism is that ports and paths are **functions, not literals**:

```hcl
service "pkg" "postgres" {
  package = "asdf:mise-plugins/mise-postgres@16.4"

  vars = { port = net::pickport() }          # resolved per run, never pinned

  runtime {
    cmd = ["postgres", "-D", "${fs::state()}/pgdata", "-p", "${self.vars.port}"]
  }

  readiness { tcp { port = self.vars.port } }
}

service "go" "api" {
  src { path = "." }

  vars = { port = net::pickport() }

  runtime {
    env = { DATABASE_URL = "postgres://127.0.0.1:${service.pkg.postgres.vars.port}/app" }
    cmd = ["${fs::bin()}/api", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness { http { path = "/healthz", port = self.vars.port } }
}
```

Nothing in that file names a port number, and `api` reads Postgres's port from Postgres rather than from a convention both sides have to remember.
So a second copy needs no port map, no project name and no reservation table:

```sh
zordon workspace create alice
cd workspaces/alice
zordon start --agent
```

`alice` gets its own state dir, its own `net::pickport()` draw for every service, and its own per-service git worktrees on `zordon/alice/<svc>`.
`bob` beside it gets another.
Neither knows the other exists, and `zordon workspace rm alice` takes it all back.

The agent drives its own copy and no other, because `zordon mcp` resolves the stack from its working directory the same way the CLI does — start it in `workspaces/alice` and that is the only stack it can reach.

## Where this is the wrong tool

Host processes give you isolation *by construction*, not *by enforcement*.
If you need an agent that genuinely cannot touch the host filesystem or network, you need a kernel boundary and you should use one.
Those two compose, incidentally: the agent stays in its container and drives the host stack over [HTTP MCP](../../how-to/drive-a-host-stack-from-a-container.md), so the sandbox is real and the stack is still cheap.

And if you run one agent at a time against one stack, none of this is your problem.
Worktrees are enough. The gap only opens when the number goes up.

---

- [Run parallel coding agents against isolated stacks](../../how-to/run-parallel-agents-with-workspaces.md) — the step-by-step version
- [Workspaces](../../workspaces.md) — the model underneath
- [Port conflicts with parallel coding agents: five fixes, ranked](parallel-agent-port-conflicts.md)
