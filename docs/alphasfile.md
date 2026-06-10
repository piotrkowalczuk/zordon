# Alphasfile

The `Alphasfile` is a single HCL2 document. Each service is a two-label
block: `service "<toolchain>" "<name>" { ... }`. Toolchain is `go`,
`rust`, `ruby`, or `nodejs` — they differ in how the binary is built
and run.

```hcl
service "go" "nats-server" {
  git {
    url = "github.com/nats-io/nats-server"   # zordon-owned bare clone
    tag = "v2.14.0"
    # exe defaults to "." (main package at repo root)
  }

  arguments = {
    p = 9010
    m = 9011
  }
}

service "rust" "tansu" {
  crate {
    name = "tansu"
  }
  features = ["server"]
}

service "go" "prometheus" {
  git {
    url = "github.com/prometheus/prometheus"
    tag = "v3.11.3"
  }
  src { exe = "./cmd/prometheus" }   # main package, relative to the repo root
  doubleDash = true

  arguments = {
    "config.file"        = "prometheus.yml"
    "log.format"         = "json"
    "web.listen-address" = ":9020"
  }

  readiness {
    http {
      path = "/-/ready"
      port = 9020
    }
  }
}

service "go" "my-app" {
  src {
    path = "~/code/my-app"   # your own checkout; zordon never writes to the primary
    exe  = "."
  }

  runtime {
    cmd = ["${fs::bin()}/my-app", "-addr", ":8080"]
  }
}
```

### Source: `git { }`, `src { }`, `crate { }`

A service picks exactly one primary. The rest comes from toolchain
defaults — keep the manifest small.

- **`git { url = "host/owner/repo" }`** — remote source. zordon
  bare-clones once into `~/.zordon/src/...`; each invocation gets a
  fresh `git worktree`. `branch` / `tag` / `rev` pin the revision.
- **`src { path = "/path" }`** — local checkout used in place (no
  clone, edit→start loop). Relative paths resolve against the
  **Alphasfile's directory**. `path` is interpolated, so it can be
  derived from the host environment, e.g.
  `src { path = "${os::env("MONOREPO")}/services/api" }`. Outside a live
  invocation (`zordon worktree`) only host-level helpers like `os::env`
  are available; `fs::`/`cfg::`/`self.*` need a running instance.
- **`src { exe = "..." }`** — the build target subdir (Go: the main
  package), relative to the source root. Default `.`. Can ride
  alongside `git { }` (subdir within the cloned worktree) **or**
  inside `src { path, exe }` for a local checkout.
- **`crate { name, version, index, registry, git, branch, tag, rev }`**
  — rust-only. `cargo install <name>` from a registry or a git URL.
  Mutually exclusive with `src` / `git` blocks. See
  [Rust services](services/rust.md) for the full field list.

The build is the toolchain
default run **in the checkout** — Go: `go build` of `exe` into
`fs::bin()` (out-of-tree, so it never dirties a `src` worktree); Rust:
`cargo build --release`; Ruby: `bundle install`. Override it with
`build { cmd = [...] }` (see [Phases](#phases-build-runtime-agent)).
It runs from the checkout; with no `runtime { cmd }` zordon runs the
built binary, and `runtime { cmd = [...] }` is an explicit argv override
(needed only when the toolchain has more than one way to run it, e.g.
`bundle exec ...` or `caddy run ...`).

This is what makes parallel **worktrees** possible — see
[Worktrees](worktrees.md).

### Flags / arguments

`arguments` is a map of flag name → value. By default flags are rendered
`-key=value` (Go-style); set `doubleDash = true` for `--key=value`, and
`space_separated = true` for `-key value` (Ruby is always space-separated).
Quote keys that contain dots: `"config.file" = "..."` — bare dotted keys
parse as nested objects in HCL2.

### Phases: build / runtime / agent

`build`, `runtime` and `agent` are full lifecycle phases (not generic
containers). `build` and `runtime` each take a `cmd` (argv list — no
implicit shell) and an `env` map (interpolated, DAG-ordered like any
other field). `agent` takes only `env`.

```hcl
service "go" "app" {
  src { path = "../.." }

  build {
    env = { BUILD_TAG = "release" }
    # argv; wrap in sh -lc when you need a shell (here, $BUILD_TAG)
    cmd = ["sh", "-lc", "go build -ldflags \"-X main.BuiltBy=$BUILD_TAG\" -o ${fs::bin()}/app ./cmd/app"]
  }

  runtime {
    env = { LOG_LEVEL = "info" }
    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  agent {
    env = { LOG_LEVEL = "error" }   # only when `zordon --agent`
  }
}
```

- **`build`** — `cmd` is the build command (omit the block for the
  toolchain default; an explicit `cmd` is exec'd as argv, no shell).
  `build.env` is injected only while building (`go build` /
  `cargo install` / `bundle`) and does **not** reach the running
  process — bake what you need in at build time (e.g. ldflags).
- **`runtime`** — `cmd` is the service argv (there is no top-level
  `cmd`); `runtime.env` is the running process env.
- **`agent`** — `env` only (it starts nothing). Overlaid on top of
  *both* build and runtime env, but only when alpha was started with
  `zordon --agent`. Use it so an automated/AI caller can e.g. quiet a
  service without editing the Alphasfile.

Layering (later wins): `env {}` (service-wide base) → the phase's
`build`/`runtime` env → `agent` env (in `--agent` mode). This is
independent of the **process/dotenv** chain documented in
[Lifecycle](lifecycle.md), which still feeds the running process.

### Readiness probes

A `readiness { http { ... } }` block makes alpha mark a service ready only
once its HTTP endpoint replies with 2xx/3xx.

```hcl
readiness {
  http {
    path   = "/-/ready"
    port   = 9020
    host   = "127.0.0.1"   # optional, defaults to 127.0.0.1
    scheme = "http"        # optional, "http" (default) or "https"
  }
  initial_delay     = "0s"
  period            = "200ms"
  timeout           = "1s"
  failure_threshold = 30
  success_threshold = 1
}
```

If no `readiness` block is set, alpha treats a service as ready once it has
stayed alive for `--stabilization` (default `1s`).

### Log control

```hcl
service "ruby" "ruby-service" {
  ...
  log {
    format = "plain"        # or "json"; structured logs get parsed
    filter = "^\\tfrom .*"  # regex of lines to drop
  }
}
```

### Debugger

`debugger { enabled = true }` runs a Go service under a headless
[Delve](https://github.com/go-delve/delve) DAP server so IDEs and
agents can attach without restarting the stack. It is a *macro* over
existing fields, not a parallel mechanism — at Alphasfile evaluation
time it expands into:

- `toolchain.go.tools` += `github.com/go-delve/delve/cmd/dlv@latest`
  and `github.com/go-delve/mcp-dap-server@latest` (unless either is
  already pinned explicitly — explicit wins),
- the toolchain-default `go build` gains `-gcflags='all=-N -l'` so
  breakpoints land on the right source lines (no optimizer
  reordering, no inlined frames),
- the runtime argv is wrapped in
  `dlv exec --headless --listen=127.0.0.1:<port> --api-version=2 --accept-multiclient [--continue] [--log] -- <built-binary> <flags>`,
- a synthetic MCP feature `debug` (bridge `dap`, address
  `127.0.0.1:<port>`) is attached to the service — visible via
  `zordon get service.<tc>.<svc>.agent.mcp.debug.address` and
  consumed by the (forthcoming) zordon-as-MCP-server bridge.

```hcl
service "go" "app" {
  src {
    path = "../.."
    exe = "./cmd/app"
  }

  vars = { port = net::pickport() }

  # No `runtime { cmd = … }` — the macro synthesizes it, and an
  # explicit cmd is rejected at validation time (use `arguments` for
  # flags, or set `wrap_runtime = false` to keep your own cmd).
  arguments = {
    addr = "127.0.0.1:${self.vars.port}"
  }

  debugger {
    enabled = true                              # the only required field
    # port            = 2345                    # pin a specific port (default: kernel-picked)
    # wait_for_client = false                   # --continue (default)
    # log             = false                   # --log
    # mcp             = true                    # emit agent.mcp.debug
    # wrap_runtime    = true                    # synth runtime.cmd
  }
}
```

Discover the resolved port after `zordon start`:

```sh
zordon get service.go.app.debugger.port
zordon get service.go.app.agent.mcp.debug.address    # 127.0.0.1:<port>
```

Field defaults and effects:

| field             | default                | effect                                                                                                          |
| ----------------- | ---------------------- | --------------------------------------------------------------------------------------------------------------- |
| `enabled`         | `false`                | required toggle; everything below applies only when `true`                                                      |
| `port`            | kernel-picked free port| dlv's `--listen` TCP port; pin a literal (e.g. `port = 2345`) when an IDE config needs a stable, known port     |
| `wait_for_client` | `false`                | when true, dlv halts at entry until a client attaches (omits `--continue`)                                      |
| `log`             | `false`                | pass `--log` to dlv for self-diagnosis                                                                          |
| `mcp`             | `true`                 | emit the synthetic `agent.mcp.debug` feature                                                                    |
| `wrap_runtime`    | `true`                 | synthesize `runtime.cmd`; set false when you launch dlv from your own script                                    |

Validation errors raised at Alphasfile parse time:

- `enabled = true` + explicit `runtime { cmd = … }` (and
  `wrap_runtime = true`) — the macro can't apply if the user already
  wrote a cmd. Use `arguments = {…}` or set `wrap_runtime = false`.
- `enabled = true` on a non-Go service — only `service "go"` is
  supported today; other toolchains will follow.

Cross-links: flags pass through [`arguments`](#flags--arguments)
unchanged; the toolchain tool installs join [`toolchain.tools`](#phases-build-runtime-agent)
under the same pinned Go.
