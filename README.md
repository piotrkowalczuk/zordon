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

Create an `Alphasfile` in your project root (see [examples/simple/Alphasfile](examples/simple/Alphasfile)),
then:

```sh
zordon start              # spawn alpha, push config, stream bringup logs
zordon status             # what's running across the whole chain right now?
zordon stop               # ask alpha to shut its children down and exit
zordon sudo               # apply the idempotent privileged hooks (see Federation)
zordon worktree create x  # a parallel, isolated copy of the stack (see Worktrees)
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
  git = "github.com/nats-io/nats-server"   # zordon-owned bare clone
  tag = "v2.14.0"
  # default build: go build -o ./nats-server .

  arguments = {
    p = 9010
    m = 9011
  }
}

service "rust" "tansu" {
  crate = "tansu"   # no git/dir ⇒ not worktree-able; expected on $PATH
}

service "go" "prometheus" {
  git        = "github.com/prometheus/prometheus"
  tag        = "v3.11.3"
  build      = "go build -o ./prometheus ./cmd/prometheus"
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
  dir = "~/code/my-app"   # your own checkout; zordon never writes to it
  command = ["./my-app", "-addr", ":8080"]
}
```

### Source: the primary repo

Every service has a **primary** — exactly one of:

- **`git = "host/owner/repo"`** — zordon bare-clones it once into
  `~/.zordon/src/...` and `git worktree add`s a fresh tree per invocation.
- **`dir = "/path/to/repo"`** — your own git checkout; zordon only
  `git worktree add`s from it, never writes to the primary.
- **neither** (e.g. `crate = "..."`, or a prebuilt binary) — not
  worktree-able; resolved from `$PATH`.

`branch` / `tag` / `rev` pin the revision checked out into the worktree.
The service is built **in that checkout** with a per-toolchain default
(`go build -o ./<name> .`, `cargo build --release`, `bundle install`)
unless you override it with `build = "..."`. It then runs from the
checkout — `command = [...]` is the full argv (cwd = checkout); with no
`command`, zordon runs `./<name> <flags>`.

This is what makes parallel **worktrees** possible — see
[Worktrees](#worktrees).

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
  git        = "github.com/prometheus/prometheus"
  tag        = "v3.11.3"
  build      = "go build -o ./prometheus ./cmd/prometheus"
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

## Worktrees

Every run of zordon happens in a *worktree*. The project root is the
implicit worktree `main`; you can spin up more, each a fully isolated
copy of the whole stack over the **same `Alphasfile`** — its own state
dir, its own `pickport()` draws, its own per-service `git worktree`
checkouts, its own `alpha`.

```sh
zordon worktree create feature        # mkdir .zordon/worktrees/feature
cd .zordon/worktrees/feature
zordon start                          # walks up, adopts ../../../Alphasfile
                                      # as the leaf — but as worktree "feature"
zordon worktree list
zordon worktree rm feature
```

`zordon start` from `.zordon/worktrees/<name>/` walks up, finds the
project's `Alphasfile`, and adopts it as the leaf — same file on disk,
different invocation. The invocation hash is
`sha(invocation_dir + alphasfile_bytes + parent_context)`, so `main` and
`feature` get **distinct hashes ⇒ distinct sockets, state dirs, and
fresh `pickport()` values** — they run side by side without colliding.
`zordon status` shows which worktree each level is and its hash; that
hash is also what `pathhash()` returns (handy for collision-free vhost
names: `app.${pathhash()}.test`).

Each worktree-able service is materialized via `git worktree add` from
its primary into `<state>/src/<svc>` and built there, so editing code in
one worktree's checkout doesn't touch another's. Federation parents
(below) are *reused* across worktrees as-is — only the leaf forks.

Main use case: an AI agent gets a sandbox next to the developer's stack;
derivatives: parallel experiments, A/B-testing two revisions. For async
isolation, keep the broker (NATS/Kafka/…) in the **project** Alphasfile,
not the global parent — then each worktree gets its own bus.

## Federation

A project rarely needs its stack in isolation. Shared infrastructure —
a reverse proxy, a local DNS resolver, a registry — belongs *above* the
project, used by many. zordon models this as a chain of Alphasfiles.

On `zordon start`, zordon walks up from the invocation directory to
`$HOME`, collecting every `Alphasfile` it finds, and (if present) the
optional global `~/.zordon/Alphasfile`. The chain is brought up
**root-first**:

```
~/.zordon/Alphasfile        (optional, global)
  └─ ~/Alphasfile           (anything found while walking up)
       └─ …/project/Alphasfile   (invocation — where you ran zordon)
```

Rules:

- **Only the invocation Alphasfile is restarted unconditionally.** Levels
  above it are *verified*: if a healthy alpha is already serving that
  Alphasfile, it's reused as-is.
- **Drift auto-restarts a parent.** zordon binds each level's config to a
  hash of (source bytes + the parent context that fed it). If you edit a
  parent Alphasfile — or a grandparent restarts with new ports — the hash
  changes and zordon restarts that level (and the cascade continues
  downward). Untouched levels keep running.
- **One start at a time per Alphasfile.** Each level is guarded by an
  exclusive `flock` (`<stateDir>/start.lock`), acquired strictly
  top-down so concurrent `zordon start`s can't deadlock.
- **`zordon stop` only stops the invocation.** Parents are shared; a
  child doesn't get to tear them down.
- **`zordon status` reports the whole chain** — every level, whether it's
  running, and the services under each.

### Privileged hooks: `zordon sudo`

Some wiring needs root (e.g. pointing macOS's resolver at a local
CoreDNS). zordon never escalates during `start` — that stays
non-interactive. Instead, a service declares idempotent `sudo` blocks,
applied on demand by `zordon sudo`:

```hcl
service "go" "coredns" {
  vars = { dns = net::pickport() }
  # …
  sudo "resolver" {
    check  = "grep -qxF 'port ${self.vars.dns}' /etc/resolver/zordon.com 2>/dev/null"
    apply  = <<-EOT
      mkdir -p /etc/resolver && printf 'nameserver 127.0.0.1\nport ${self.vars.dns}\n' > /etc/resolver/zordon.com
    EOT
    verify = "grep -qxF 'port ${self.vars.dns}' /etc/resolver/zordon.com"
  }
}
```

`zordon sudo` walks the chain, reads each *running* alpha's resolved
steps (so snippets carry the ports services actually bound to), and for
each step:

1. runs `check` **without** sudo — if it exits 0 the step is already
   satisfied and is skipped, **no password prompt**;
2. otherwise runs `apply` as `sudo /bin/sh -c …` (one prompt, wired to
   your terminal);
3. optionally runs `verify` without sudo to confirm.

Because `check` gates `apply`, re-running `zordon sudo` in steady state
prompts for nothing. A step only re-applies when its inputs changed —
e.g. CoreDNS came back on a different port.

The federation example uses two such hooks:

- **coredns/resolver** — writes `/etc/resolver/test` so macOS sends
  `*.test` lookups to the local CoreDNS (CoreDNS serves the whole `.test`
  TLD; projects pick collision-free names via `pathhash()`, e.g.
  `prometheus.<hash>.test`).
- **caddy/http80** — fronts Caddy on `:80` without running it as root:
  Caddy stays on its unprivileged pickport and a `pf` rule redirects
  loopback `:80` to it. This one-time-edits `/etc/pf.conf` to add an
  `rdr-anchor "zordon"` (a backup is kept at `/etc/pf.conf.zordon.bak`).
  Revert with:

  ```sh
  sudo pfctl -a zordon -F all
  sudo sed -i '' '/rdr-anchor "zordon"/d' /etc/pf.conf
  sudo pfctl -f /etc/pf.conf
  ```

So the full loop is: `zordon start` (chain up, services on pickports) →
`zordon sudo` (DNS + `:80` wired) → `curl http://prometheus.<hash>.test/`
resolves via CoreDNS, hits Caddy on `:80` via pf, proxies to the
project. The `:80` and resolver hooks are macOS-specific; on Linux you'd
use `CAP_NET_BIND_SERVICE` / systemd-resolved instead.

### Context flows down the chain

Every resolved service is exposed to the levels below it under the same
flat `service.<toolchain>.<name>` namespace — a child can't tell whether
`service.go.caddy` lives in its own Alphasfile or three levels up. Names
must be unique across the whole chain (a collision is an error).

This is how a project wires itself into shared infra without hardcoding
anything, and without any per-app glue code. From
[examples/federation](examples/federation):

```hcl
# examples/federation/Alphasfile  (the root: caddy + coredns)
service "go" "caddy" {
  git   = "github.com/caddyserver/caddy"
  tag   = "v2.10.0"
  build = "go build -o ./caddy ./cmd/caddy"
  vars = {
    http       = net::pickport()
    config_dir = "${tmpdir()}/conf.d"
  }
  file "caddyfile" {
    path = "${tmpdir()}/Caddyfile"
    body = "…  import ${self.vars.config_dir}/*.caddy"
  }
  command = ["./caddy", "run", "--config", self.file.caddyfile.path,
             "--adapter", "caddyfile", "--watch"]
}

# examples/federation/project/Alphasfile  (the project)
service "go" "prometheus" {
  git    = "github.com/prometheus/prometheus"
  tag    = "v3.11.3"
  build  = "go build -o ./prometheus ./cmd/prometheus"
  vars   = { port = net::pickport() }

  # The entire integration: one dropped vhost fragment.
  file "caddy_vhost" {
    path = "${service.go.caddy.vars.config_dir}/prometheus.caddy"
    body = <<-EOT
      http://prometheus.local.zordon.com:${service.go.caddy.vars.http} {
      	reverse_proxy 127.0.0.1:${self.vars.port}
      }
    EOT
  }
  # … arguments to run prometheus on self.vars.port …
}
```

The project never talks to Caddy. It just writes a `*.caddy` fragment
into the directory Caddy `--watch`es (discovered through the chain as
`service.go.caddy.vars.config_dir`); Caddy hot-reloads and
`prometheus.local.zordon.com` starts proxying. `zordon start` from
`examples/federation/project` ensures Caddy + CoreDNS are up one level
up, then brings the project up with the parent's resolved port and
config dir injected — a complete loop with zero hardcoded ports and zero
registration code. (Prometheus is a stand-in for any OSS Go web app;
only the fragment is project-specific.)

`command` (a list expression, evaluated after `vars`/`file`/`arguments`)
overrides the default `<name> <flags>` invocation — needed for
subcommand-driven binaries like `caddy run`.

## License

[GNU General Public License v3.0](https://www.gnu.org/licenses/gpl.txt), see [LICENSE](LICENSE.md).
