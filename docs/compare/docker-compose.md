---
title: "Docker Compose alternative for local development"
description: "Docker Compose isolates by image and pays cold start on every stack. Zordon runs the same services as host processes so a second copy is a process tree — a comparison of both trades."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/compare/docker-compose/">https://zordon.io/compare/docker-compose/</a></div>

# Docker Compose alternative for local development

Docker Compose is the default for a reason: one file describes the whole stack, it runs identically on every machine, and the isolation is genuine.
If your stack comes up in the morning and goes down in the evening, that is a very good deal and there is nothing here to fix.

zordon makes a different trade, and it only pays off under a specific condition: when you create and destroy stacks *often*.

## The difference in one line

Compose isolates by **image**; zordon isolates by **run**.

Everything below follows from that.

## Side by side

| | Docker Compose | zordon |
|---|---|---|
| Unit | container from an image | supervised host process |
| Isolation | kernel namespaces | separate ports, state dirs and checkouts, per run |
| Cost of the *second* stack | another full set of containers | another process tree |
| Cold start | image pull/build, then container start | build cache, then `exec` |
| Idle overhead | per-container, always | the process itself |
| Ports | static in the file, or published at random | `net::pickport()`, resolved per run and readable by other services |
| Cross-service config | env vars you keep in sync by hand | `service.pkg.postgres.vars.port` — read from the source |
| Host parity | normalised away | used directly |
| Ships to production | yes, the same images | no, and deliberately so |

## What Compose is better at

- **Reproducibility across machines.** An image pins the OS and everything in it. zordon pins versions through [mise](https://mise.jdx.dev) and then runs on your host, which is a weaker guarantee.
- **Anything you also ship.** If the container is the deliverable, running it locally is free information. zordon builds no images and cannot give you that.
- **Enforced isolation.** A container cannot see your home directory unless you mount it. A host process can.
- **You already have it working.** A Compose file that runs is worth more than an argument.

## What zordon is better at

- **The second, third and tenth copy.** `zordon workspace create alice` gives a whole extra stack with its own ports and state; the marginal cost is process memory. Standing up three Compose projects costs three full stacks.
- **Cold start inside a loop.** Bringup is a build cache lookup and an `exec`, so it fits inside the iteration rather than around it.
- **Ports that resolve themselves.** No port map to maintain, no `-p` project names to invent, and no published-port collisions to work around — services read each other's ports from the config graph.
- **Convergent re-runs.** Re-running costs only what actually changed, instead of a teardown and rebuild.
- **Native host tooling.** Your debugger, profiler and editor attach to a normal process on your machine. No exec-into-the-container step.
- **An interface an agent can be given.** [`zordon mcp`](../reference/mcp.md) exposes the stack — and only the operations you declared — as MCP tools, so an agent gets a narrow surface instead of a shell.

## The case that decides it

Three coding agents on one project.

With Compose you run `docker compose -p alice up`, `-p bob`, `-p carol`.
It works; you pay three cold starts and carry three idle stacks, and you still have to solve host-port collisions — pin them and they fight, randomise them and you have to discover what you got.

With zordon:

```sh
zordon workspace create alice
cd workspaces/alice && zordon start --agent
```

Each workspace draws its own ports, gets its own state dir, and checks out its own per-service worktrees.
Nothing is pinned, so nothing collides.
`zordon workspace rm alice` takes it back.

The step-by-step version is in [Run parallel coding agents against isolated stacks](../how-to/run-parallel-agents-with-workspaces.md).

## You do not have to choose

They compose, in the literal sense.
Keep the agent in a container for a real security boundary and let it drive the host stack over [HTTP MCP](../how-to/drive-a-host-stack-from-a-container.md) — the sandbox stays a sandbox, and the stack stays cheap.

## Next

- [Getting started](../getting-started.md) — a real two-service stack in a few minutes
- [Alphasfile](../alphasfile.md) — what replaces `docker-compose.yml`
- [Dynamic configuration](../dynamic-config.md) — why ports are functions here
