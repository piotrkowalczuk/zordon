# Zordon

Zordon runs the supporting stack for local agentic workflows — the
databases, queues, brokers, and toy services an agent needs to actually
exercise your code while it works.

The constraint is resourcefulness. An agent loop spins these dependencies
up and tears them down constantly; every second of cold start and every
megabyte of idle overhead is paid back many times a day. Containers solve
isolation by paying that cost upfront — fine for production, wasteful here.
Zordon runs the same services as plain host processes, supervised
together, declared once in an `Alphasfile`.

## Installation

```sh
go install github.com/piotrkowalczuk/zordon/cmd/...@latest
```

This installs both `zordon` and `alpha` into your `$GOBIN` (or
`$GOPATH/bin`). Make sure that directory is on your `$PATH`.

## Quick start

Create an `Alphasfile` in your project root (see [example/Alphasfile](example/Alphasfile)),
then:

```sh
zordon start              # spawn alpha, push config, stream bringup logs
zordon status             # what's alpha running right now?
zordon stop               # ask alpha to shut its children down and exit
```

`zordon start` exits as soon as every service reaches READY (so you can
keep using your shell); `alpha` keeps running in the background until you
`zordon stop` it or kill it.

### Useful flags

```
zordon --agent              machine-friendly log format ('<ms> <src> <LEVEL> <msg>')
zordon start --failfast     abort bringup and shut down on first failure
zordon start --alpha-log P  where alpha appends its own log
zordon start --timeout 60s  max wait for alpha to come up & finish bringup
```

## Alphasfile

The `Alphasfile` is a single HCL2 document. Each service is a two-label
block: `service "<toolchain>" "<name>" { ... }`. Toolchain is `go`,
`rust`, or `ruby` — they differ in how the binary is built and run.

```hcl
service "go" "nats-server" {
  import = "github.com/nats-io/nats-server/v2"
  branch = "v2.14.0"

  arguments = {
    p = 9010
    m = 9011
  }
}

service "rust" "tansu" {
  crate        = "tansu"
  all_features = true
}

service "go" "prometheus" {
  import     = "github.com/prometheus/prometheus/cmd/prometheus"
  branch     = "v3.11.3"
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
    period            = "200ms"
    timeout           = "1s"
    failure_threshold = 30
  }
}

service "ruby" "ruby-service" {
  git     = "github.com/niwasawa/ruby-sinatra-hello-world"
  install = "bundle install --path vendor/bundle"
  run     = "bundle exec ruby myapp.rb"

  arguments = {
    p = 8888
  }
}
```

### Toolchain blocks

- **`service "go" "<name>"`** — `import` is a Go import path; the binary is
  expected on `$PATH` after `go install <import>`. `branch` pins a tag.
- **`service "rust" "<name>"`** — installed via `cargo install`. With
  `git = "..."`, zordon clones and builds from a local checkout
  (`cargo install --path`).
- **`service "ruby" "<name>"`** — `git` points at a repo, `install` runs
  after clone, and `run` is the command line to start the service. The
  whole flow is driven from the cloned checkout: zordon clones into
  `~/.zordon/src/<host>/<owner>/<repo>`, runs `install` there, then runs
  `run` with `cwd` set to that directory.

### Flags / arguments

`arguments` is a map of flag name → value. By default flags are rendered
`-key=value` (Go-style); set `doubleDash = true` for `--key=value`, and
`space_separated = true` for `-key value` (Ruby is always space-separated).
Quote keys that contain dots: `"config.file" = "..."` — bare dotted keys
parse as nested objects in HCL2.

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

## Dynamic configuration

The `Alphasfile` is evaluated as a DAG before bringup: expressions in one
service can reference values computed in another, and zordon orders the
evaluation so dependencies resolve first. This is what makes the local
stack reproducible — no shell glue to wire ports together, no `.env`
files committed by hand.

Everything that belongs to a service lives inside its block: locally-cached
values (`vars`), generated files (`file "<name>" { ... }`), and the
arguments / readiness probe that consume them.

### Helpers

- `net::pickport()` — returns a free TCP port (binds to `:0`, closes,
  reports the port). Each call returns a new port — store it in `vars`
  if you need to reuse it.
- `tmpdir()` — returns a deterministic per-Alphasfile directory under
  `$TMPDIR/zordon-<sha8>/`. Same value every time you call it during a
  single evaluation. Created on demand.

### `self` inside a service

Inside a service, `self` exposes the service's own resolved values. It
fills in incrementally as evaluation progresses through these stages:

1. `self.{name, toolchain, dir}` — known immediately from the block labels
   and `import`/`git`.
2. `self.vars` — populated after `vars = { ... }` is evaluated.
3. `self.file.<name>` — `{ path, body }` of each nested `file` block,
   added as they're evaluated.
4. `self.arguments` — populated after `arguments = { ... }`.

The `readiness.http.port` expression runs last, so it sees all of the above.

### Cross-service references

After a service is fully evaluated, the same data is exposed under
`service.<toolchain>.<name>` for downstream services:

- `service.go.foo.vars.<key>`
- `service.go.foo.arguments["<key>"]`
- `service.go.foo.file.<name>.path`
- `service.go.foo.dir`

The DAG ensures the referenced service is evaluated first.

### Example

```hcl
service "go" "prometheus" {
  import     = "github.com/prometheus/prometheus/cmd/prometheus"
  doubleDash = true

  vars = {
    port = net::pickport()
  }

  file "config" {
    path = "${tmpdir()}/prometheus.yml"
    body = <<-EOT
      global:
        scrape_interval: 15s
      scrape_configs: []
    EOT
  }

  arguments = {
    "config.file"        = self.file.config.path
    "web.listen-address" = ":${self.vars.port}"
  }

  readiness {
    http {
      path = "/-/ready"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 30
  }
}
```

One `pickport()` call lands in `vars`; the same port flows into the
listen address, the readiness probe, and (if needed) any other service
referencing `service.go.prometheus.vars.port`. The config file is
materialized into `tmpdir()` at configure time and unlinked when
`zordon stop` shuts alpha down.


## License

[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl.txt), see [LICENSE](LICENSE.md).
