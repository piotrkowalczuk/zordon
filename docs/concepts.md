# Concepts

Most of what shapes a run never appears in the Alphasfile. These are the
implicit entities the system is built from — the things you reason in.

## The chain

There is no "list of Alphasfiles" anywhere. The chain is **discovered by
position**: from the directory you ran zordon in, zordon walks upward to
`$HOME`, and every `Alphasfile` it passes is a level, plus the optional
global `~/.zordon/Alphasfile`. Root-most first, your project last.

The chain is the unit of federation: a project doesn't import shared
infra, it *sits below* it. Move the project deeper or shallower in the
tree and its environment changes, because the chain is recomputed from
where it now lives.

→ [Federation](federation.md)

## The level (an alpha)

Each position in the chain has its own `alpha` supervising its own
services. A **level** is that unit — one Alphasfile, one supervisor, one
lifecycle. Levels above the invocation are *reused* as they are; only
the invocation level is yours to restart. "Shared infra" isn't a flag —
it's just a level other runs also point at.

→ [Architecture](architecture.md)

## The evaluation context

An Alphasfile is never read in isolation — it is *evaluated against a
context*. That context is assembled, not written: zordon asks each
already-running parent alpha for its **resolved** services and folds
them, level by level, into one flat `service.<toolchain>.<name>`
namespace. By the time your file is evaluated, the context already
holds every concrete port, directory and generated path the levels
above produced.

This is why a child can say `service.go.caddy.vars.http` and get a real
number with no coordination: the value didn't come from the parent's
*file*, it came from the parent's *running, resolved state* collected
into the context.

→ [Dynamic configuration](dynamic-config.md)

## The invocation

A run's identity is not the file — it's **where you ran it**. The
invocation is that resolved identity: a hash over the invocation
directory, and from it a state dir, sockets, build outputs and
per-service checkouts. Two runs of the same Alphasfile from different
directories are different invocations, hence fully disjoint.

So the Alphasfile is a template; an invocation (a worktree) is one
concrete instantiation of it. "A run" — not "the file" — is the noun
everything else hangs off.

→ [Worktrees](worktrees.md)

## Service and toolchain

A **service** is uniform above the build line: a process, an
environment, a readiness condition, a place in the dependency graph.
Everything there is identical whatever the language.

A **toolchain** is the abstraction that absorbs everything *below* that
line — where the source is and how a binary comes to exist. Go resolves
a package and `go build`s it; Rust is a crate or a cargo workspace and
goes through `cargo install`; Ruby is a bundler tree with no single
binary. These models genuinely differ, so the toolchain, not the
service, carries the difference: `git`/`src`/`crate`, `exe`/`bin`,
default build, run shape. Pick the toolchain and the divergence is
contained; everything the rest of the system touches is the same shape.

→ [Services](services/go.md)

## Functions and the graph

Expressions are not strings. `net::pickport()`, `fs::tmp()`,
`pathhash()`, `os::env()` are **functions evaluated during resolution** —
some have identity (a port is drawn once and reused everywhere it's
referenced), some are pure coordinates of the invocation. They turn a
static document into a description that adapts to *this* run.

Because values reference each other, order is not the file's order — it
is a **graph**. Inside a service `vars`, each `file`, `arguments`,
`env`, `dotenv`, and the `build`/`runtime`/`agent` phase `cmd`/`env`
are sequenced by what they actually reference; across services by
`service.*` references. You declare
relationships; the graph decides evaluation order, and a true cycle is
an error, never a hang.

→ [Dynamic configuration](dynamic-config.md)

## fs:: coordinates

A run has an implicit filesystem shape you address symbolically, never
by hardcoded path: `fs::src()` (this service's checkout), `fs::bin()`
(build outputs, deliberately outside the checkout), `fs::tmp()`
(scratch / generated files). They resolve per invocation, which is what
keeps two worktrees from ever writing over each other.

→ [Dynamic configuration](dynamic-config.md)
