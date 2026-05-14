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

The `Alphasfile` is a single HCL document that declares one block per
service. Each block is namespaced by toolchain — `service.go`,
`service.rust`, `service.ruby` — because the install/build path differs.

```hcl
service.go "nats-server" {
  import = "github.com/nats-io/nats-server/v2"
  branch = "v2.14.0"

  arguments {
    p = 9010
    m = 9011
  }
}

service.rust "tansu" {
  crate        = "tansu"
  all_features = true
}

service.go "prometheus" {
  import     = "github.com/prometheus/prometheus/cmd/prometheus"
  branch     = "v3.11.3"
  doubleDash = true

  arguments {
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

service.ruby "ruby-service" {
  git     = "github.com/niwasawa/ruby-sinatra-hello-world"
  install = "bundle install --path vendor/bundle"
  run     = "bundle exec ruby myapp.rb"

  arguments {
    p = 8888
  }
}
```

### Toolchain blocks

- **`service.go`** — `import` is a Go-style import path; the binary is
  expected on `$PATH` after `go install <import>`. `branch` pins a tag.
- **`service.rust`** — installed via `cargo install`. With `git = "..."`,
  zordon will clone and build from a local checkout (`cargo install --path`).
- **`service.ruby`** — `git` points at a repo, `install` runs after clone,
  and `run` is the command line to start the service. The whole flow is
  driven from the cloned checkout: zordon clones into `~/.zordon/src/<host>/<owner>/<repo>`,
  runs `install` there, then runs `run` with `cwd` set to that directory.

### Flags / arguments

Each service can declare an `arguments` block. Keys are flag names, values
are passed verbatim. By default flags are rendered `-key=value` (Go-style);
set `doubleDash = true` for `--key=value`, and `space_separated = true` for
`-key value` (Ruby is always space-separated).

If a flag name contains a dot (`web.listen-address`), quote the key so HCL
treats it as one string and not a nested block.

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
service.ruby "ruby-service" {
  ...
  log {
    format = "plain"        # or "json"; structured logs get parsed
    filter = "^\\tfrom .*"  # regex of lines to drop
  }
}
```


## License

[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl.txt), see [LICENSE](LICENSE.md).
