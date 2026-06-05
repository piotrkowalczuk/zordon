# Overview

## What is Zordon?

Zordon is a supervisor for the supporting stack that local **agentic and
development workflows** run against — the databases, brokers, proxies,
and services your code needs alive to be exercised for real. It exists
to make that stack **cheap to stand up, cheap to duplicate, and cheap to
throw away**, because the way software is now written makes those the
properties that matter most.

## Why it exists

Code is increasingly written in a loop: an agent (or a developer driving
one) changes something, runs the real stack against it, reads the
result, and goes again — hundreds of times an hour. In that world the
dominant cost is no longer how the stack is built; it's how often you
pay for it.

That reframes everything. A few seconds of cold start and a few hundred
megabytes of idle overhead look harmless once. Multiplied across a tight
loop, and across several loops running at once, they become the whole
experience. The supporting stack stops being background infrastructure
and becomes the thing standing between an idea and its feedback.

Zordon is the bet that a local stack should be optimized for that loop
first.

## Resourcefulness as the design axis

Every choice in Zordon is bent toward spending as little as possible per
iteration:

- **Latency over abstraction.** Bringup should feel free, so it can sit
  inside a loop rather than around it.
- **Duplication over coordination.** A second copy of the whole stack —
  an agent's sandbox beside your own — should be a non-event, not a
  port-mapping exercise.
- **Convergence over restart.** Re-running should cost only what
  actually changed, not the whole stack.
- **Signal over noise.** When the consumer of your logs is a model,
  tokens are a budget. Output is kept terse and structured — what
  happened, not pages of boilerplate — so a loop spends its context on
  the problem, not on scrollback.
- **Reproducibility over ceremony.** The same description must produce
  the same environment, every time, without human glue.

Containers buy isolation by paying the full latency-and-overhead cost
upfront, on every run. That trade is right for production and wrong for
a loop. Zordon takes the other side: ordinary supervised host processes,
with isolation recovered from per-run worktrees instead of from images.

## Where it fits

Zordon is for the inner loop — iterating on code against a live,
multi-service stack, giving an agent a disposable environment next to
yours, running experiments in parallel. It is deliberately *not* a
production runtime, a scheduler, or a container platform; where you need
kernel-level isolation or to ship opaque images, use the tools built for
that.

## Where next

- [Getting started](getting-started.md) — install and a first stack
- [Concepts](concepts.md) — the mental model that makes the rest obvious
- [Architecture](architecture.md) · [Alphasfile](alphasfile.md) · [Worktrees](worktrees.md) · [Federation](federation.md)
- [`zordon mcp` reference](reference/mcp.md) · [Run a provision via MCP](how-to/run-a-provision-via-mcp.md) — drive zordon (and its provisions) from an agent over MCP
