# Lifecycle

What happens between `zordon start` and a READY stack, and how
reconfigure / stop behave.

## `zordon start`

1. **Discover chain** — walk up collecting Alphasfiles (root-first),
   plus optional `~/.zordon/Alphasfile`.
2. **Per level, top-down:**
   1. Build the `Invocation` (hash, state dir, tmp dir, sockets).
   2. Take the level's `flock`.
   3. Probe a running `alpha` on the socket.
      - **Healthy + same config hash** (non-invocation level) → reuse,
        accumulate its resolved services into the parent context, move
        on.
      - **Drift** (hash changed) or **the invocation level** → ask the
        old alpha to stop, wait for the socket to go away.
   4. Resolve the Alphasfile with the accumulated parent context.
   5. Spawn `alpha`, wait for its READY handshake.
   6. Send **Configure** (resolved services, config hash, parent
      dotenv) and stream events until `Done`.
   7. Accumulate this level's services → context for the next level.
3. Locks release in reverse order; `zordon` detaches. `alpha`
   processes keep running in the background.

## Per-service bringup (inside alpha)

For each service in `Configure` order:

1. **Skip if unchanged** — already running and neither its code nor
   manifest changed since the last Configure → left untouched (another
   service changing does *not* rebuild it).
2. **Prepare**
   - `git`/`src`: ensure the primary (bare-clone on first use), then
     `git worktree add` into `…/src/<svc>` on branch `zordon/<wt>`
     (sparse cone if a `workspace { sparse = … }` block is set).
   - `crate`: no checkout.
   - **Build** the toolchain default (or `build { cmd = [...] }`
     override) into `fs::bin()` — see [Services](services/go.md).
3. **Materialize files** — `file{}` blocks written atomically
   (temp + rename).
4. **Start** the process (`runtime { cmd }` if given, else the built
   binary), cwd = the service working dir (`<checkout>/<exe>`),
   environment composed (see below).
5. **Readiness** — a `readiness` probe (`http {…}`, `exec {…}`, or
   `tcp {…}`) gates READY; without one, "alive for `--stabilization`"
   (default `1s`) counts as ready.
6. Stdout/stderr stream back to `zordon` as events. On `--failfast`
   (default on) a failed bringup aborts the rest and shuts the level
   down.

## Environment precedence

Lowest to highest, last wins:

1. `alpha`'s process environment, **filtered by `sysenv`** (closed-world
   whitelist — anything not listed is stripped; defaults to empty)
2. **toolchain env** — `mise env --json` output for the pinned
   `toolchain.<lang>.version`, with per-language pin reinforcement
   (e.g. `GOTOOLCHAIN=local` for Go), then the user's
   `toolchain.<lang>.env` overlay
3. federation-parent **file-level** `dotenv` (root-first)
4. this Alphasfile's file-level `dotenv`
5. the service's `dotenv`
6. the service's `env { }` block, then the matching phase's `env { }`
   (`runtime`/`build`), and finally `agent { env { } }` when alpha
   runs in `--agent` mode

The toolchain is the **initial** environment: it lays down PATH /
GOROOT / GEM_PATH / GOTOOLCHAIN / etc. so language tooling resolves to
the pinned install. Per-service config (`dotenv`, `env`, phase env)
layers on top, so users override toolchain defaults — never the other
way around.

All of `dotenv`/`env` are interpolated and DAG-ordered like any other
field.

## Reconfigure

A second `zordon start` on a still-running level sends a fresh
Configure. `alpha` diffs it: services missing or whose resolved config
changed are stopped (`stopServices`: SIGTERM → grace → SIGKILL, only
those), new/changed services are brought up, **untouched services keep
running** — a change in one service never restarts another. Rust keeps
a stable `CARGO_TARGET_DIR` so rebuilds stay incremental.

## Picking a subset

`zordon start <svc> [...]` filters the **invocation level** down to the
named services plus the transitive closure of their `runtime.after` and
`provision.after` deps — federation parents are unaffected and always
come up in full (shared infra). Anything not in the closure is omitted
from the Configure payload, so on reconfigure `alpha` treats those as
"removed from the manifest" and stops them. A subsequent
`zordon start` with no picks brings the rest back up. Unknown picks
fail before contacting alpha and list the available service names.

`zordon plan <svc> [...]` accepts the same picks and renders exactly
that subset statically — a preflight of "just what I'm about to start"
without spawning alpha. Parents still render in full.

The selection can equally come from the `--services` flag or the
`ZORDON_SERVICES` env var (comma- or space-separated), merged with any
positional args. The env var is the only channel for injecting a subset
where you control the environment but not argv — e.g. from a Claude Code
hook driving `zordon`.

## `zordon sudo`

Out-of-band, never during `start`. Walks the chain, reads each running
alpha's resolved `sudo` steps, and per step runs `check` (no sudo) —
skip if it passes — else `apply` via `sudo` then optional `verify`.
Idempotent: steady state prompts for nothing. See
[Federation](federation.md).

## `zordon stop`

Stops **only the invocation level** (parents are shared infra). `alpha`
SIGTERMs every child, waits the grace period, SIGKILLs stragglers, then
unlinks the generated files and exits. `zordon status` reports the
whole chain regardless.

`zordon stop --clean` is sugar for `zordon stop` followed by `zordon clean`:
it brings the stack down, waits for it to actually exit, then runs the
teardown snippets (see below).

## `zordon clean`

Runs each provision's [`clean`](dynamic-config.md#provision-teardown-clean)
teardown snippet for the **invocation level** — the inverse of the side
effects bringup created (drop a database, delete a topic, remove generated
files). Like `zordon stop`, shared parent levels are left untouched.

Clean operates on a **stopped** stack. A still-running stack is refused
(`zordon stop` first, or use `zordon stop --clean`). Under the hood it spawns
a transient `alpha` that materializes toolchains and rebuilds each service's
env/cwd **without starting the services**, runs the `clean` snippets in
**reverse declaration order** (teardown is the inverse of bringup), then shuts
that `alpha` down. A failed clean is reported but does not abort the rest.

Provisions without a `clean` snippet are skipped. Snippets that reference a
provision's invoke-time `arguments` are run as-is — there is no per-invoke
argument context during a batch clean.
