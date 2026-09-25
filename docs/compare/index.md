---
title: "How zordon compares"
description: "Where zordon sits next to Docker Compose and container-per-agent setups for local development — what each is good at, and when zordon is the wrong tool."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/compare/">https://zordon.io/compare/</a></div>

# How zordon compares

zordon runs the databases, brokers, proxies and services your code needs alive as ordinary supervised **host processes**, declared once in an [`Alphasfile`](../alphasfile.md).
It optimises for one thing: making a copy of the whole stack cheap enough that you stop rationing them.

## When it is the wrong tool

Worth saying first, because it decides most of these comparisons.

- **Production, or anything that ships.** zordon is an inner-loop tool. It builds no images and schedules nothing.
- **You need a kernel boundary.** Host processes give isolation by construction — separate ports, separate state dirs, separate checkouts — not enforcement. An agent that must be *unable* to touch the host needs a container or a VM.
- **You want the host normalised away.** Containers hide the host; zordon uses it. That is the trade in both directions.
- **One developer, one long-lived stack.** If you bring the stack up on Monday and stop it on Friday, per-stack cost is not your problem and none of this matters.

## The comparisons

- [vs Docker Compose](docker-compose.md) — the closest thing to a default, and the one most people are moving from
- [vs a container per agent](devcontainers.md) — devcontainers, agent sandboxes, and why the two actually compose

## The short version

Compose and devcontainers buy isolation by paying full cold-start and idle cost on every stack.
That is the right trade when a stack is long-lived, and the wrong one when stacks are created and thrown away inside a loop — which is what running several coding agents in parallel looks like.
zordon takes the other side and recovers isolation from [per-run workspaces](../workspaces.md) instead of from images.
