# Alphasfile

The `Alphasfile` is a single HCL2 document. Each service is a two-label
block: `service "<toolchain>" "<name>" { ... }`. Toolchain is `go`,
`rust`, or `ruby` — they differ in how the binary is built and run.

```hcl
service "go" "nats-server" {
  git = "github.com/nats-io/nats-server"   # zordon-owned bare clone
  tag = "v2.14.0"
  # exe defaults to "." (main package at repo root)

  arguments = {
    p = 9010
    m = 9011
  }
}

service "rust" "tansu" {
  exe = "tansu"   # no git/src ⇒ not worktree-able; expected on $PATH
}

service "go" "prometheus" {
  git        = "github.com/prometheus/prometheus"
  tag        = "v3.11.3"
  exe        = "./cmd/prometheus"   # main package, relative to the repo root
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
  src = "~/code/my-app"   # your own checkout; zordon never writes to the primary
  exe = "."

  runtime {
    cmd = ["${fs::bin()}/my-app", "-addr", ":8080"]
  }
}
```

### Source: three pointers

A service is described by three location pointers (the rest comes from
toolchain defaults — keep the manifest small):

- **`git = "host/owner/repo"`** — the repo. zordon bare-clones it once
  into `~/.zordon/src/...`; each invocation gets a fresh `git worktree`.
- **`src = "/path"` / `"../.."`** — a local checkout to use as the
  primary instead of cloning `git`. Relative values resolve against the
  **Alphasfile's directory**. zordon only `git worktree add`s from it —
  never writes to your primary. Give `git` *or* `src`, not both.
- **`exe`** — the build target (Go: the main package), **relative to the
  primary root** (the `src` dir, or the git-clone root). Default: `.`;
  set it when the main lives elsewhere (e.g. `./cmd/foo`). With neither
  `git` nor `src`, `exe` is just a binary name resolved from `$PATH`.

`branch` / `tag` / `rev` pin the revision. The build is the toolchain
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
  src = "../.."

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
