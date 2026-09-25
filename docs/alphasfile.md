---
description: "The HCL2 document declaring every service — block labels, source shapes, arguments, readiness probes, provisions and generated files."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/alphasfile/">https://zordon.io/alphasfile/</a></div>

# Alphasfile

The `Alphasfile` is a single HCL2 document. Each service is a two-label
block: `service "<toolchain>" "<name>" { ... }`. Toolchain is `go`,
`rust`, `ruby`, `nodejs`, or `pkg` — they differ in how the binary is
sourced, built, and run (`pkg` runs a prebuilt native package via mise
rather than building anything; see [Package services](services/pkg.md)).

```hcl
service "go" "nats-server" {
  git {
    url = "github.com/nats-io/nats-server"   # zordon-owned bare clone
    tag = "v2.14.0"
    # exe defaults to "." (main package at repo root)
  }

  arguments {
    values = {
      main = {
        p = 9010
        m = 9011
      }
    }
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

  arguments {
    values = {
      main = {
        "config.file"        = "prometheus.yml"
        "log.format"         = "json"
        "web.listen-address" = ":9020"
      }
    }
    options {
      prefix = "--"
    }
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
  invocation (`zordon workspace`) only host-level helpers like `os::env`
  are available; `fs::`/`cfg::`/`self.*` need a running instance.
- **`src { exe = "..." }`** — the build target subdir (Go: the main
  package), relative to the source root. Default `.`. Can ride
  alongside `git { }` (subdir within the cloned workspace) **or**
  inside `src { path, exe }` for a local checkout.
- **`crate { name, version, index, registry, git, branch, tag, rev }`**
  — rust-only. `cargo install <name>` from a registry or a git URL.
  Mutually exclusive with `src` / `git` blocks. See
  [Rust services](services/rust.md) for the full field list.

The build is the toolchain
default run **in the checkout** — Go: `go build` of `exe` into
`fs::bin()` (out-of-tree, so it never dirties a `src` workspace); Rust:
`cargo build --release`; Ruby: `bundle install`. Override it with
`build { cmd = [...] }` (see [Phases](#phases-build-runtime-agent)).
It runs from the checkout; with no `runtime { cmd }` zordon runs the
built binary, and `runtime { cmd = [...] }` is an explicit argv override
(needed only when the toolchain has more than one way to run it, e.g.
`bundle exec ...` or `caddy run ...`).

This is what makes parallel **workspaces** possible — see
[Workspaces](workspaces.md).

### Flags / arguments

`arguments` is a block with two parts:

- **`values = { … }`** — **named groups**, each a flag name → value map
  (interpolated; joins the dependency DAG). A value is reachable as
  `self.arguments.values.<group>.<key>`. Most services need a single group
  (call it whatever, e.g. `main`); subcommand-driven tools split flags into
  several (e.g. `global`, `serve`).
- **`options { … }`** — how groups render into argv. Two knobs, each an enum
  that matches a real CLI convention:
  - **`prefix`** — `"-"` (default, Go-style) · `"--"` (GNU long) · `"/"`
    (Windows) · `"+"` · `""` (none).
  - **`separator`** — `"="` (default) · `" "` (space → two argv elements,
    `--flag value`) · `":"` (Windows, `/flag:value`) · `""` (glued,
    `-O2`). Ruby always separates by space regardless of this setting.

```hcl
arguments {
  values = {
    main = {
      "config.file"        = "prometheus.yml"
      "web.listen-address" = ":9020"
    }
  }
  options {
    prefix = "--"          # → --config.file=prometheus.yml
  }
}
```

**Placement.** With **no `runtime.cmd`**, every group is rendered and
appended after the binary (groups in name order, keys sorted within each —
deterministic). With an **explicit `runtime.cmd`** nothing is auto-appended;
you place each group yourself with `tpl::render::flags("<group>")`, whose
rendered tokens are spliced (flattened) into the argv in place. That covers
the `program <globals> subcommand <sub-flags>` shape:

```hcl
arguments {
  values = {
    global = { debug = true }
    serve  = { config = self.file.c.path }
  }
  options { prefix = "--" }
}
runtime {
  cmd = ["caddy", tpl::render::flags("global"), "run", tpl::render::flags("serve")]
}
# → caddy --debug=true run --config=/…
```

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

A `readiness` block makes alpha mark a service ready only once a probe passes.
The block carries exactly one action — `http`, `exec`, or `tcp` — plus the
shared timing knobs (`initial_delay`, `period`, `timeout`, `failure_threshold`,
`success_threshold`).

The `http` action polls an endpoint and treats a 2xx/3xx reply as ready.

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

The `exec` action runs a CLI command and treats exit code 0 as ready — the
readiness equivalent of a Kubernetes exec probe.
Use it when the service ships its own readiness check, e.g. Postgres' `pg_isready`.
`command` is the argv (no implicit shell; use `["sh", "-c", "..."]` for one)
and is interpolated, so it can reference `self.vars` and peers.
`env` overlays the probe process environment.

```hcl
readiness {
  exec {
    command = ["pg_isready", "-h", "127.0.0.1", "-p", "${self.vars.port}"]
    env     = { PGUSER = "zordon" }   # optional, overlays the inherited env
  }
  period            = "200ms"
  failure_threshold = 30
}
```

The `tcp` action dials a port and treats a successful connection as ready —
for databases and brokers that don't speak HTTP. It takes a `port` (and
optional `host`, default `127.0.0.1`); see
[Package services](services/pkg.md#readiness-tcp) for a worked example.

```hcl
readiness {
  tcp { port = self.vars.port }
  period            = "200ms"
  failure_threshold = 30
}
```

If no `readiness` block is set, alpha treats a service as ready once it has
stayed alive for `--stabilization` (default `1s`).

### Log control

```hcl
service "ruby" "ruby-service" {
  ...
  log {
    format = "plain"  # or "json"; structured logs get parsed
    # A boolean predicate over each line; true drops the line, at the
    # source, before it reaches the output or alpha's log.
    filter = <<-EOT
      hasPrefix(line, "\tfrom ") or (contains(line, "level") and severity(line) <= DEBUG)
    EOT
  }
}
```

`filter` is a small DSL — `contains`/`hasPrefix`/`matches` on the raw line, `json`/`logfmt`/`severity` for structured fields, combined with `and`/`or`/`not`.
See the [log filter reference](reference/log-filter.md).

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
  arguments {
    values = {
      main = {
        addr = "127.0.0.1:${self.vars.port}"
      }
    }
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
  wrote a cmd. Use `arguments { values = {…} }` or set `wrap_runtime = false`.
- `enabled = true` on a non-Go service — only `service "go"` is
  supported today; other toolchains will follow.

Cross-links: flags pass through [`arguments`](#flags--arguments)
unchanged; the toolchain tool installs join [`toolchain.tools`](#phases-build-runtime-agent)
under the same pinned Go.

## Top-level `workspace {}` — preparing a workspace directory

Alongside the `service` blocks, one top-level `workspace {}` block says how to
prepare a workspace *directory*: which files to put in it, and what to call the
branch each picked service is checked out on.

It is a different block from the service-level `workspace { sparse }`, which
says how to cut one service's checkout. Same word, different scope.

```hcl
workspace {
  branch = "zordon/${workspace.name}/${service.name}"   # the default

  file "claude" {
    path = "CLAUDE.md"
    create { source = "templates/CLAUDE.md" }
  }

  file "devcontainer" {
    path = ".devcontainer/devcontainer.json"
    create { body = enc::json({ name = "zordon-${workspace.name}" }) }
  }

  file "settings" {
    path = ".claude/settings.json"
    merge { data = { env = { ZORDON_WORKSPACE = workspace.name } } }
  }

  file "ignore" {
    path = ".gitignore"
    region { body = ".devcontainer/" }
  }
}
```

Each `file` block takes a `path` and exactly one operation, which is what
decides who owns the file:

| Operation | Ownership | Payload |
| --- | --- | --- |
| `create` | zordon owns the whole file | `body` or `source` (exactly one) |
| `merge` | zordon owns a fragment of a structured file you own | `data`, plus optional `format` |
| `region` | zordon owns a marked span of a text file you own | `body` or `source`, plus optional `comment` |

`merge` applies [RFC 7386](https://www.rfc-editor.org/rfc/rfc7386) JSON Merge
Patch to JSON, YAML or TOML: objects merge key by key, any other value replaces
what was there, and a `null` deletes its key. `format` is inferred from the
path's extension. Arrays are replaced wholesale rather than appended to — that
is what makes a repeated apply a no-op, and the trade is that a patch owns a
list or does not touch it. YAML and TOML round-trip through Go libraries that
drop comments and original formatting; JSON has neither to lose.

`region` maintains a `# >>> zordon: <name>` … `# <<< zordon: <name>` span,
appending it when absent and replacing it when present. `comment` defaults to
`#`. It is refused on `.json`, which has no comment syntax — use `merge` there.

`source` names a file inside the project root. It is rendered as an HCL
template, not copied, so it can interpolate the same values a `body` can; a
literal `${` or `%{` that is not meant as interpolation must be escaped `$${`
or `%%{`.

### When these are written, and what that rules out

The files are materialized by `zordon workspace create` and
`zordon workspace apply` — **before** anything is built or started, because an
agent or a dev container has to find them already in place.

That timing decides the evaluation context. Available:

- `workspace.name`, `.dir`, `.root`, `.hash`, `.port`
- `fs::hash()`, `fs::state()`, `fs::tmp()`, `fs::bin()`
- `os::env()`
- `enc::json()`, `enc::yaml()`, `enc::toml()`

Deliberately unavailable, each with an error naming the alternative:
`net::pickport()`, `self.*`, `service.<tc>.<svc>.*`, `src::hash()`,
`cfg::hash()`, `fs::src()`, `fs::exe()`, `fs::etc()`, `fs::var()`.

None of them can answer truthfully before the stack exists — a port drawn here
would simply not be the one alpha draws later. Values that depend on a running
service belong in that service's own [`file {}`](#generated-files) block, which
alpha materializes at start with the whole resolved graph in scope.

`workspace.port` is the exception that makes static files useful: it is derived
from `workspace.hash`, so it is stable across runs of a given workspace and can
name a server that is started afterwards.

See [Workspaces](workspaces.md) for where a generated file may not go, and for
the branch template.

### `enc::json` / `enc::yaml` / `enc::toml`

The `enc::` namespace serializes an HCL value into a document:

```hcl
body = enc::json({
  mcpServers = {
    zordon = { type = "stdio", command = "zordon", args = ["mcp", "--dir", "."] }
  }
})
```

Building a document from structure rather than from a hand-quoted heredoc is
what keeps an interpolated path or name carrying a quote or a backslash from
corrupting it silently. Every encoder sorts object keys and keeps integers
integral, so the output is a pure function of the input — which is what makes
`workspace apply` idempotent. They are available in service-level `file {}`
bodies too.
