# Dynamic configuration

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
- `fs::tmp()` — a per-invocation scratch dir under `$TMPDIR/zordon-<fs::hash>/`
  for generated files. Stable within one evaluation.
- `fs::src()` — the calling service's source checkout (its per-invocation
  `git worktree`). Same as `self.dir`.
- `fs::bin()` — the per-invocation build-output dir, deliberately
  **outside** the source checkout so building never dirties a `src`
  primary's worktree. The default Go build drops `<name>` here; reference
  it from `cmd` as `${fs::bin()}/<name>`.
- `fs::hash()` — short hash identifying this **alpha instance** by its
  filesystem location (project root + worktree). Stable across edits;
  distinct per worktree. Handy for collision-free names, e.g.
  `app.${fs::hash()}.test`.
- `cfg::hash()` — short hash of the **manifest** (Alphasfile bytes +
  resolved parent context). Changes whenever the manifest does — what
  federation drift detection compares.
- `src::hash()` — short identity of the calling service's **source code**
  (`git rev-parse HEAD` of `fs::src()`). Useful as a build-cache key or
  a `-ldflags "-X main.Tag=..."` stamp; pair with `fs::hash()` when you
  also need the location.

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
  exe        = "./cmd/prometheus"
  doubleDash = true

  vars = {
    port = net::pickport()
  }

  file "config" {
    path = "${fs::tmp()}/prometheus.yml"
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
materialized into `fs::tmp()` at configure time and unlinked when
`zordon stop` shuts alpha down.

### Reading resolved values

`zordon get <expr>` prints a single resolved value — the same numbers
the running stack actually uses (it queries the live alpha; with nothing
running it falls back to a static evaluation, exactly like `zordon
status`). Useful for wiring scripts to a `pickport()` address without
parsing logs.

The tree is keyed `service.<toolchain>.<name>.<field>`, where the fields
are the resolved runtime config: `vars`, `arguments`, `env`, `command`,
`dir`, `bin_dir`, `print`, plus live `pid`/`ready`/`running`.

```sh
zordon get service.go.prometheus.vars.address      # 127.0.0.1:9090
zordon get service.go.prometheus.command.0         # prometheus
zordon get service.go.prometheus.ready             # true
```

Anything containing `{{` is evaluated as a Go template against the same
tree (a `json` function is available for composite values):

```sh
zordon get '{{ .service.go.prometheus.vars.port }}'
zordon get '{{ json .service.go.prometheus.command }}'
```

Scalars print raw (newline-terminated, scriptable); maps and slices
print as compact JSON. An unknown path fails with the list of available
keys at that level.
