---
description: "Reference for `--build.maxcpu` and `ZORDON_BUILD_MAXCPU`: CPU quota resolution, build slot serialization, and compiler concurrency variables."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/reference/build-maxcpu/">https://zordon.io/reference/build-maxcpu/</a></div>

# `--build.maxcpu`

`--build.maxcpu` sets the CPU quota and build-slot serialization policy for heavy build and cold toolchain compilation steps managed by [`alpha`](binaries.md).

| Flag | Env | Default | Accepted by |
| --- | --- | --- | --- |
| `--build.maxcpu N` | `ZORDON_BUILD_MAXCPU` | `0` (auto) | `zordon` (global flag), `alpha run` |

`zordon` forwards `--build.maxcpu` when spawning [`alpha`](binaries.md) and inside every `Configure` and `Clean` request across the federation chain.
[`zordon mcp`](mcp.md) forwards `--build.maxcpu` to every CLI invocation it runs.

## Quota resolution

Let `cpus` be `max(1, runtime.GOMAXPROCS(0))` inside `alpha`.

| Input | Resolved quota (`Q`) |
| --- | --- |
| `0` (default) and `cpus > 4` | `cpus - 2` |
| `0` (default) and `cpus <= 4` | `cpus` |
| `N >= 1` | `N` |
| `N < 0` or non-integer | Rejected at flag or environment parse time |

`alpha` logs the resolved quota on configure as `build.maxcpu=<Q>`.

## Build slot

`alpha` maintains one FIFO build slot per supervisor process.
Only one heavy build or cold toolchain compilation holds the slot at a time, and the holder receives the full quota `Q`.

| Operation | Acquires build slot |
| --- | --- |
| Service `build` step (default toolchain build or `build { cmd }`) | Yes |
| Cold `mise` source compilation fallback (`cargo install mise`) | Yes |
| Cold `mise install <lang>@<version>` or `mise install <pkg>@<version>` | Yes |
| Cold `toolchain.<lang>.tools` install (`gem`, `pip`, `cargo`, `go`, `npm`) | Yes |
| Warm toolchain and package cache checks | No |
| Prebuilt `mise` release archive download and extraction | No |
| Source checkout (`git clone`, `git fetch`, `git worktree add`) | No |
| Service `runtime` processes, provisions, and readiness probes | No |

While an entity waits behind an active holder, its bringup phase is reported as `waiting for build slot` in [`alpha`](binaries.md) logs and periodic bringup progress output.

## Environment variables

Before spawning a subprocess that holds the build slot, `alpha` injects the resolved quota `Q` into the subprocess environment for keys that are not already set:

| Variable | Injected value | Consumer |
| --- | --- | --- |
| `CARGO_BUILD_JOBS` | `<Q>` | Cargo (`rust` builds and `cargo install`) |
| `BUNDLE_JOBS` | `<Q>` | Bundler (`ruby` gem installation) |
| `MAKEFLAGS` | `-j<Q>` | GNU Make and native C/C++ extensions |
| `CMAKE_BUILD_PARALLEL_LEVEL` | `<Q>` | CMake builds |
| `GOMAXPROCS` | `<Q>` | Go toolchain and `go build` |

Existing entries in the subprocess environment take precedence and are never overwritten.
An explicit variable in `build { env }`, `toolchain.<lang>.env`, or a whitelisted `sysenv` entry in the [`Alphasfile`](../alphasfile.md) wins over the injected value; see [Environment precedence](../lifecycle.md#environment-precedence).
For OS scheduling priority (`nice`), which applies unconditionally across `alpha`'s entire process tree independently of `--build.maxcpu`, see [Process priority](binaries.md#process-priority).
