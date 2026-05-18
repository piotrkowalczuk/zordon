# Federation

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
[examples/federation](https://github.com/piotrkowalczuk/zordon/tree/master/examples/federation):

```hcl
# examples/federation/Alphasfile  (the root: caddy + coredns)
service "go" "caddy" {
  git = "github.com/caddyserver/caddy"
  tag = "v2.10.0"
  exe = "./cmd/caddy"
  vars = {
    http       = net::pickport()
    config_dir = "${fs::tmp()}/conf.d"
  }
  file "caddyfile" {
    path = "${fs::tmp()}/Caddyfile"
    body = "…  import ${self.vars.config_dir}/*.caddy"
  }
  cmd = ["${fs::bin()}/caddy", "run", "--config", self.file.caddyfile.path,
         "--adapter", "caddyfile", "--watch"]
}

# examples/federation/project/Alphasfile  (the project)
service "go" "prometheus" {
  git    = "github.com/prometheus/prometheus"
  tag    = "v3.11.3"
  exe    = "./cmd/prometheus"
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

`cmd` (a list expression, evaluated after `vars`/`file`/`arguments`)
overrides the default run — needed for subcommand-driven binaries like
`caddy run …`. The built binary lives in `fs::bin()`.
