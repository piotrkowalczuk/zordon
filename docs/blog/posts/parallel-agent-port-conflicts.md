---
title: "Port conflicts with parallel coding agents: five fixes, ranked"
description: "Two agents, one port 3000. Five ways to fix it — staggering, per-worktree .env files, ephemeral ports, a Compose project per agent, declared dynamic ports — with the cost of each."
slug: parallel-agent-port-conflicts
date: 2026-09-01
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/blog/parallel-agent-port-conflicts/">https://zordon.io/blog/parallel-agent-port-conflicts/</a></div>

# Port conflicts with parallel coding agents: five fixes, ranked

You gave each agent its own git worktree and started the second one.
It died on `listen tcp 127.0.0.1:3000: bind: address already in use`.

There are five answers to this and four of them are not a tool.
Here they are with what each actually costs.

<!-- more -->

## 1. Don't run them at the same time

Stagger the agents, or let one finish before the next starts.

Free, immediate, and genuinely the right call if you run two agents occasionally and mostly work alone.
Every other option on this list buys you concurrency, and concurrency you are not using is not worth paying for.

You have outgrown it when you find yourself waiting on an agent you could have parallelised, or when "just don't run them together" turns into a rule you have to remember at 2am.

**Isolation:** total (there is only one stack).
**Cost:** your wall-clock time.

## 2. A `.env` per worktree with hand-assigned ports

```sh
# worktrees/feature-a/.env
PORT=3001
DATABASE_URL=postgres://localhost:5432/app_feature_a

# worktrees/feature-b/.env
PORT=3002
DATABASE_URL=postgres://localhost:5432/app_feature_b
```

The cheapest fix that survives contact with reality, and for a single web process it is the correct answer.

It degrades in a specific way: the numbers are static, so the allocation is *yours* to keep consistent.
One service is a line. Five services across four worktrees is a twenty-cell table that lives in your head, drifts the moment someone adds a service, and silently collides when two agents pick the same number.
Note also that this line fixes the *port* and not the database — the two URLs above still point at one Postgres instance, so migrations and seeds still cross between agents.

**Isolation:** ports only, and only as far as your bookkeeping is correct.
**Cost:** manual, and grows as services × agents.

## 3. Bind to port 0 and let the OS choose

Ask the kernel for an ephemeral port and print what you got.
No collision is possible, because no number is ever assumed.

This is excellent for a single self-contained process and it is why so many test harnesses do it.
It breaks down as soon as something else has to *find* that process: the port is now known only at runtime, inside the process, and every consumer needs a way to learn it.
Most off-the-shelf servers you depend on — Postgres, Redis, a broker — also want a port at startup, and will not discover each other for you.

So port 0 solves allocation and leaves you with discovery.

**Isolation:** perfect for the listener, nonexistent for its dependents.
**Cost:** you now own service discovery.

## 4. A Docker Compose project per agent

```sh
docker compose -p feature-a up -d
docker compose -p feature-b up -d
```

Real isolation: separate networks, separate volumes, separate containers, separate databases.
If you already run everything in Compose, this is the smallest step from where you are, and it fixes the shared-database problem that fixes 2 and 3 do not.

Two costs.
First, published host ports still collide — `ports: "5432:5432"` in two projects fights over 5432 on the host, so you either randomise the host side (and land back in discovery) or stop publishing and reach services only from inside the network.
Second, you pay full container price per stack: image pulls, cold start, and idle memory for every copy.
That is fine when a stack lives for a day. It is the dominant cost when agents create and discard stacks all afternoon.

**Isolation:** strong.
**Cost:** per-stack cold start and idle overhead, paid every time.

## 5. Declare ports as values to be resolved, not numbers to be chosen

The reason the first four are awkward is that the port is a *constant* in a file, and constants cannot be duplicated.
The fix is to stop writing the number down.

```hcl
service "pkg" "postgres" {
  package = "asdf:mise-plugins/mise-postgres@16.4"
  vars    = { port = net::pickport() }

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

`net::pickport()` resolves at bringup, per run.
`api` reads Postgres's port *from Postgres*, so allocation and discovery are the same mechanism instead of two problems.
Paths work the same way: `fs::state()` is per-run, so the data directory forks along with the port.

A second copy of the whole thing is then a command, not a plan:

```sh
zordon workspace create alice && cd workspaces/alice && zordon start --agent
```

This is what [zordon](https://zordon.io) does, and the honest limitation is that the services are ordinary host processes.
You get isolation by construction — separate ports, separate state dirs, separate checkouts — not a kernel boundary.
If you need an agent that *cannot* reach the host rather than one that has no reason to, use option 4 or a sandbox, and note that the two compose: the agent can sit in its container and [drive the host stack over HTTP](../../how-to/drive-a-host-stack-from-a-container.md).

## Ranked

| | Isolation | Setup | Cost per extra stack | Holds at N agents |
|---|---|---|---|---|
| 1. Stagger | total | none | — | no |
| 2. `.env` per worktree | ports only | minutes | a table row per service | no |
| 3. Port 0 | listener only | small code change | none | only if you add discovery |
| 4. Compose project per agent | strong | hours | cold start + idle RAM | yes, if you can afford it |
| 5. Declared dynamic ports | ports, state, checkouts | hours | a process tree | yes |

Pick 1 if you rarely run two agents.
Pick 2 if you have one service and expect to keep it that way.
Pick 4 if you already live in Compose and your stacks are long-lived.
Pick 5 if agents create and throw away stacks often enough that per-stack cost is what you are actually optimising.

---

- [Git worktrees isolate your code. They don't isolate your stack.](git-worktrees-dont-isolate-your-stack.md)
- [Run parallel coding agents against isolated stacks](../../how-to/run-parallel-agents-with-workspaces.md)
- [Dynamic configuration](../../dynamic-config.md) — how `net::pickport()` and cross-service references resolve
