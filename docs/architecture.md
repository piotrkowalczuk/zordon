# Architecture

Two binaries, a unix-domain control socket, and a strict split between
*resolving* config (pure) and *running* it (effectful).

```
you ──► zordon (CLI)                       alpha (supervisor, per level)
        │  discover chain (walk up)        │  bind control socket
        │  resolve Alphasfile → values     │  materialize file{} blocks
        │  flock per level (top-down)      │  prepare: checkout + build
        ├── spawn ───────────────────────► │  start services, pipe logs
        │  Configure(resolved, parentEnv)  │  readiness probes
        │  ◄── Event stream (logs/ready) ──┤  compose env, reconfigure
        │  detach                          │  shutdown on stop/signal
```

## zordon — the CLI

Stateless. For one `zordon start` it:

1. **Discovers the chain** — walks the invocation dir up to `$HOME`,
   collecting every `Alphasfile`, plus the optional global
   `~/.zordon/Alphasfile`; root-first, leaf last.
2. **Builds an Invocation per level** — `Dir`, `Workspace`, `StateDir`,
   `Hash`, `TmpDir`. The leaf's identity comes from the CWD (so a run
   from `workspaces/<name>/` is that workspace); parents are
   always `main` rooted at their own dir.
3. **Resolves** each Alphasfile (pure: HCL2 parse → DAG → interpolate;
   no process spawn, no clone). Parent results feed the child's
   evaluation context.
4. **Locks** each level with an exclusive `flock`
   (`<stateDir>/start.lock`), acquired strictly top-down so concurrent
   starts can't deadlock.
5. **Reconciles** — reuse a healthy parent as-is; restart on drift
   (config hash changed) or for the invocation level.
6. **Spawns / configures** `alpha` and streams its event log until
   every service is READY, then detaches.

The resolver lives in `internal/alphasfile` and is intentionally
side-effect-free — it is unit-tested without spawning anything.

## alpha — the supervisor

One `alpha` per chain level, long-lived. It:

- binds the control socket at `Invocation.SocketPath()` (under
  `$TMPDIR`, to stay within the unix-socket path-length limit);
- on **Configure**: materializes `file{}` blocks atomically, then for
  each service runs **prepare** (git worktree / `cargo install`, then
  the toolchain build into `fs::bin()`), starts it, pipes
  stdout/stderr back as events, and runs the readiness probe;
- composes each child's environment (sysenv-filtered host → toolchain
  env → federation-parent dotenv → file-level dotenv → service
  `dotenv` → `env {}` / phase env);
- on **reconfigure**: stops only the services whose code/manifest
  changed (`stopServices`) and leaves the rest running;
- on stop/SIGTERM: SIGTERM → grace → SIGKILL every child, then unlinks
  generated files.

## Control protocol

Newline-delimited JSON over the unix socket
(`internal/protocol`): a `Request` (`Configure` / `State` / `Shutdown` /
`Invoke`), then either a single `Response` or a streamed sequence of
`Event`s (log lines, `ServiceStart`, `ServiceReady`, `ServiceFail`,
`Done`, `Error`). `Configure` carries the fully-resolved `Alphasfile`,
the config hash, and the federation parents' file-level dotenv paths.

## On-disk layout

Per invocation:

```
<projectRoot>/workspaces/<workspace>/
  ├── start.lock          # flock guarding this level
  ├── src/<service>/      # per-service git worktree (fs::src)
  ├── bin/                # build outputs           (fs::bin)
  └── cache/rust/target/  # shared CARGO_TARGET_DIR (incremental)

$TMPDIR/
  ├── zordon-<hash>/
  │   └── alpha.sock      # control socket; the dir is fs::tmp() scratch
  └── alpha-<hash>.log    # the leaf alpha's own log (--alpha-log overrides)
```

`fs::bin()` is deliberately outside `src/` so building never dirties a
`src` primary's working tree. The invocation hash makes `main` and any
workspace fully disjoint on disk.
