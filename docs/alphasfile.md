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

### Modules

A `module "<name>" { }` block is a namespace for services with an optional toolchain pin of its own.
Two modules may each declare a `service "go" "db"`; the module keeps them apart.

```hcl
toolchain {
  go { version = "1.27.0" }            # the default module's pin, and the fallback for every module
}

service "go" "gateway" {               # top-level = the default module; identity unchanged
  vars = { upstream = module.payments.service.go.api.vars.port }
  runtime { after = [module.payments.service.go.api.runtime.ready] }
}

module "payments" {
  toolchain {
    go { version = "1.22.0" }          # only payments' services run under this pin
  }

  service "go" "db" { … }
  service "go" "api" {
    vars = { db = service.go.db.vars.port }        # bare `service.*` = this module
    runtime { after = [service.go.db.runtime.ready, toolchain.go.ready] }
  }
}
```

| aspect | default module (top level) | inside `module "m"` |
|---|---|---|
| canonical id | `service.<tc>.<name>` | `module.m.service.<tc>.<name>` |
| display name (picks, `zordon status`, checkout dir, worktree branch) | `<name>` | `m/<name>` |
| bare `service.<tc>.<name>` in expressions | default module | module `m` only, no fallback |
| cross-module reference | `module.<other>.service.<tc>.<name>…` | `module.<other>.service.<tc>.<name>…` |
| `toolchain.<lang>.ready` | `toolchain.<lang>@ready` | `module.m.toolchain.<lang>@ready` for the module's own pin, else `toolchain.<lang>@ready` |
| `fs::bin()` and build output | `<state>/bin`, artifact `<name>` | `<state>/bin/m`, artifact `<name>` (a block moved into a module keeps `${fs::bin()}/${self.name}` unchanged) |
| state dirs (`fs::etc()`, `fs::var()`) | `<state>/etc/<name>` | `<state>/etc/m/<name>` |
| `zordon get` path | `service.<tc>.<name>.…` | `module.m.service.<tc>.<name>.…` |
| MCP provision tool | `provision__<tc>_<name>__<step>` | `provision__<tc>_m_<name>__<step>` |

Rules:

- A module name is a plain identifier (`[A-Za-z][A-Za-z0-9_-]*`) and is declared once per file.
- Services are unique per `(module, toolchain, name)`, so the same name in two modules is not a duplicate.
- Inside a module the default module and federation parents are not addressable; the entrypoint composes modules, modules do not reach up.
- `self.module` holds the declaring module's name (empty at top level), handy for `-name "${self.module}/${self.name}"` style flags.
- `fs::service::bin`, `fs::service::etc` and `fs::service::var` accept `module.<m>.service.<tc>.<name>` references.
- A module's `toolchain { }` pins are keyed `<m>/<lang>` in the resolved manifest and the plan renders them inside the module block.
- `env`, `dotenv` and `sysenv` stay top-level only; module-scoped env is not supported.

See [Pin different toolchain versions per module](how-to/pin-different-toolchain-versions.md) for the recipe and [examples/modules](https://github.com/piotrkowalczuk/zordon/tree/main/examples/modules) for a runnable stack.

### Imports

An `import` block pulls named modules out of another file into the stack.
At the entrypoint's top level it composes the stack; inside a module it declares what that module depends on.
A module is the only unit of import; a file is just a container that may hold several modules.

```hcl
# Alphasfile (entrypoint)
import "./services/apps/Alphasfile.apps" {
  modules = ["app", "billing"]
}

# services/apps/Alphasfile.apps (fragment)
module "app" {
  import "../kafka/Alphasfile.kafka" {
    modules = ["kafka"]
  }

  service "go" "app" {
    runtime {
      provision "topic" {
        cmd = module.kafka.service.go.kafka.runtime.provision.create-topic
      }
    }
  }
}
module "billing" { … }

# services/kafka/Alphasfile.kafka (fragment)
module "kafka" { service "go" "kafka" { … } }
```

| rule | behavior |
|---|---|
| syntax | `import "<path>" { modules = ["<m>", …] }`, repeatable; `modules` is required and non-empty for a fragment |
| placement | the entrypoint's top level, a `module` block in any file, and a `package` block |
| path | starts with `./`, `../`, `/` or `~/`; relative paths resolve against the declaring file's directory; any other spelling is a [remote identity](#remote-imports) |
| entrypoint vs fragment | a file named exactly `Alphasfile` is an entrypoint and cannot be imported as a file; import its directory to use it as a [package](#packages), or a fragment, by convention `Alphasfile.<name>` |
| fragment content | `module` blocks and `require` blocks only; a top-level `import`, `service`, `toolchain`, `env`, `dotenv`, `sysenv` and `workspace` are errors |
| loading | every file loads once; diamonds and cycles between files are fine; imports inside modules outside the stack are loaded and checked too |
| stack | the entrypoint's modules, the modules its imports name, and, repeated until nothing changes, the modules imported by any module already in the stack; every other module is left out |
| visibility at the entrypoint's top level | the entrypoint's modules and the modules its imports name |
| visibility inside a module | the module itself and what it imports; in the entrypoint also every other module of the entrypoint, because all of them are in the stack |
| sibling in a fragment | imported like any other module, by the fragment's own file name: `import "./Alphasfile.apps" { modules = ["billing"] }` |
| visibility error | names the expression and the missing `import` and where it goes |
| duplicates | a module name is declared once across all loaded files |
| relative `src { path }` | resolves against the directory of the file that declares the service |
| `cfg::hash()` and drift | covers the bytes of every loaded file, so editing a fragment restarts a level like editing its Alphasfile |
| `zordon plan` | prints `# import <file> [modules]` per file the stack takes modules from and `# unused module <m> in <file>` under the level header |

An import inside a module joins the stack only with that module, so taking one module from a shared file never starts what its neighbours need.

See [Split an Alphasfile across files](how-to/split-an-alphasfile-across-files.md) for the recipe and [examples/import](https://github.com/piotrkowalczuk/zordon/tree/main/examples/import) for a runnable stack.

### Packages

A package is the API between a stack and the modules that implement it.
Its file holds one `package` block: the package's inputs, features and imports, and its modules.

```hcl
# caddy/Alphasfile
package "caddy" {
  inputs   = { domain = "test", http_port = null }
  features = { coredns = "Routes hosts by name, resolved through CoreDNS." }

  import "../coredns" {
    enabled = features.coredns
    inputs  = { zone = inputs.domain }
  }

  module "caddy" {
    service "go" "caddy" { … }
  }
}

# Alphasfile
import "./caddy" "edge" {
  features = ["coredns"]
  inputs   = { domain = "dev" }
}
```

| rule | behavior |
|---|---|
| file | a package's `Alphasfile` holds exactly one `package "<name>" {}` block and nothing outside it |
| block content | `inputs`, `features`, `import`, `require`, `toolchain` and `module` blocks; `env`, `dotenv`, `sysenv`, `workspace` and services outside a module are errors |
| target | an import whose path resolves to a directory whose `Alphasfile` holds a package block; a directory without one is an error |
| name | the alias label when given, else the package block's label; unique among packages in the stack |
| identity | a module of a package is `package.<p>.module.<m>`; its services are `package.<p>.module.<m>.service.<tc>.<svc>`, shown as `<p>/<m>/<svc>` |
| inside a package | `module.<m>` is one of the package's own modules; another package is `package.<q>.module.<m>` |
| import at the entrypoint's top level | the final word on the package's inputs and features; every other import of the package must be satisfied by it |
| any other import | configures the package when the entrypoint does not import it |
| features from several imports | unite; every importer gets what it needs |
| an input from several imports | an error: an input has one source; set it where the entrypoint imports the package |
| unmet need | a feature the entrypoint leaves off, or an input it sets to another value, is an error on the entrypoint's import that quotes the feature's description and names the importer |
| entrypoint imports | several imports of the same package at the entrypoint's top level must pass the same inputs and features |
| cycle | packages that import each other cannot be configured first; importing one of them at the entrypoint's top level breaks the cycle |
| dependencies | a package depends on packages, never on a fragment's modules |
| toolchain | the package's `toolchain {}` pins every module that has none of its own |
| visibility | a package's modules see each other and whatever the package or the module imports |
| federation | importing a package that is a federation level of the invocation is an error, because it would run twice |
| on its own | `zordon start` in the package's directory runs it with default inputs and no features; a required input is an error |
| `zordon plan` | prints `# import <dir> as <name> [features: …] (imported by …)` and renders `package "<p>" { module "<m>" { … } }` |

### Inputs and features

```hcl
package "coredns" {
  inputs   = { zone = "test", token = required }
  features = { resolver = "Writes /etc/resolver/<zone> with sudo, so a browser resolves the zone." }

  module "coredns" {
    service "go" "coredns" {
      sudo "resolver" {
        enabled = features.resolver
        apply   = "… ${inputs.zone} …"
      }
    }
  }
}
```

| rule | behavior |
|---|---|
| `inputs = { <n> = <default> }` | declares the package's inputs; read as `inputs.<n>`; `null` is an ordinary default |
| `required` | an input with no default; whoever configures the package must pass it |
| input values | defaults and imported values are evaluated before planning: literals, `os::env`, `enc::*` and, in a package's import, the package's own `inputs`; never another service's values |
| unknown input | passing an input the package does not declare is an error listing the declared ones |
| `features = { <n> = "<description>" }` | declares the package's features; the description says what the feature turns on and is quoted in errors; all are off unless an import lists them; read as `features.<n>`, a bool |
| unknown feature | an error listing the declared ones |
| `enabled` | on `service`, `file`, `provision`, `sudo` and `import` blocks inside a package; a false value removes the block before planning |
| `enabled` expressions | `features.<n>`, `!`, `&&`, `||` only; no functions and no other variables |
| reference to a removed block | an error on the referencing block that says which `enabled` removed the target and asks to gate the reference the same way |
| `enabled` outside a package | an error |

A service's process gets no host variables unless the entrypoint passes them with `sysenv`.
Without `HOME`, zordon points Go's caches at `$ZORDON_HOME/go`, so a package builds without anything from the host.

### Remote imports

An import path that does not start with `./`, `../`, `/` or `~/` is an identity: a repository followed by a path inside it.
The version lives in a `require` block, not in the path.

```hcl
require "github.com/piotrkowalczuk/zordon" { ref = "v1.4.0" }

import "github.com/piotrkowalczuk/zordon/examples/package/caddy" {}
```

| rule | behavior |
|---|---|
| identity | `<host>/<owner>/<repo>[/<path>]`; hosts github.com, gitlab.com and bitbucket.org; `@<ref>` in a path is an error that shows the `require` to write |
| `require "<repo>" { ref = "<ref>" }` | pins a repository to a branch, a tag or a commit; the label is the repository alone |
| where requires live | in the nearest `zordon.mod` above the file; a file with no `zordon.mod` above it declares them itself, at its top level or inside its package block |
| `require` under a `zordon.mod` | an error: the versions of a module live in its `zordon.mod` |
| missing require | an error that names the file to add it to, or `zordon pkg get <repo>@<ref>` |
| own repository | a file fetched from a repository that names that repository reads the same checkout and needs no require |
| one version per repository | the entrypoint's require wins; without one, two requires of the same repository with different refs are an error that asks to pin it for the entrypoint |
| resolution order | a `search` entry of the applying `zordon.work`, then the `zordon.lock` pin, then a fetch; the require is needed in every case |
| checkout | `$ZORDON_HOME/mod/<host>/<owner>/<repo>@<commit>`, reused without the network once present |
| confinement | a file of a remote checkout may import relative paths only, and never outside its checkout |
| `cfg::hash()` | records `<repo>@<commit>//<path>` for remote files, so moving a pin restarts the level |
| `zordon plan` | appends `(<repo>@<commit>)` or `(search <dir>)` to every remote import |
| `zordon pkg get <repo>@<ref>` | writes the require where it belongs and locks it |
| `zordon pkg update [repo ...]` | moves the pins in `zordon.lock` to the newest commits of their refs |

See [zordon.work, zordon.mod and zordon.lock](reference/files.md) for the three files, [Use a package from another repository](how-to/use-a-package-from-another-repository.md), [Develop a dependency locally](how-to/develop-a-dependency-locally.md), and [examples/package](https://github.com/piotrkowalczuk/zordon/tree/main/examples/package) for a runnable stack.

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
