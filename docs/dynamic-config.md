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

`self` is the alias for **this service** — fixed for the entire `service`
block, *not* a "current scope" that changes inside nested blocks. So
`self` in a `provision`, `file`, `sudo`, or `runtime` block all point at
the same outer service object. There's no provision-scoped `self`;
nested blocks inherit the service's `self`. Pragmatically you almost
always want service-level state (vars, ports, dir) from inside any of
those nested blocks, so this saves a level of indirection.

`self` fills in incrementally as evaluation progresses through stages
of the service:

1. `self.{name, toolchain, dir}` — known immediately from the block labels
   and `import`/`git`.
2. `self.runtime.*` — barrier surface for the service's lifecycle
   (see "Barriers" below). Available from the start; the values are
   reference strings, the actual events fire at runtime.
3. `self.vars` — populated after `vars = { ... }` is evaluated.
4. `self.file.<name>` — `{ path, body }` of each nested `file` block,
   added as they're evaluated.
5. `self.arguments` — populated after `arguments = { ... }`.

The `readiness.http.port` expression runs last, so it sees all of the above.

### Barriers

A **barrier** is a synchronization point: an event that fires once and
never un-fires, that other entities can wait on. Every service and every
provision has a set of barriers attached to its lifecycle.

Reference paths mirror HCL block nesting (the rule is
**self-discoverable**: if you can write the block, you can guess the
reference). Service runtime states hang off `self.runtime`; provisions
hang off `self.runtime.provision.<name>` because that's where you
declare them in HCL.

#### Service runtime barriers

These mirror the long-running daemon's lifecycle:

| Path | Fires when |
|---|---|
| `self.runtime.scheduled` | service is queued for bringup; `after` deps not yet checked |
| `self.runtime.running` | the service's `cmd` process has been spawned |
| `self.runtime.ready` | readiness probe (or stabilization timer) passed |
| `self.runtime.stopped` | process exited cleanly after being ready |
| `self.runtime.done` | outcome decided — `ready` *or* terminal failure |

There's also `self.runtime.failed` (terminal failure during bringup) but
it isn't exposed in HCL on purpose: waiting on it is "run on failure",
which is a different orchestration pattern (cleanup hook) than the
forward-progress dependencies that `after` expresses. When a dep enters
a terminal failure, waiters on its `ready` barrier are unblocked
automatically and inherit the failure — no deadlock, no need to
explicitly list `failed`.

#### Service build barriers

Every service has a one-shot build phase (a checkout or a `cargo
install` or a `go install` — see [Alphasfile](alphasfile.md#service-block)).
Its barriers are exposed alongside `self.runtime`:

| Path | Fires when |
|---|---|
| `self.build.scheduled` | build is queued, deps not yet checked |
| `self.build.running` | the build cmd is executing |
| `self.build.success` | build cmd exited 0 (or there was no build to run) |
| `self.build.done` | outcome decided — `success` *or* `failure` |

Use `self.build.success` (or the cross-service form below) to gate
tooling that operates on the built artifact BEFORE the runtime is
exposed — e.g. a smoke check on the binary, a static-analysis step, a
publish to a registry. Without a distinct build barrier you'd have to
wait for `runtime.ready`, which means the service is already live.

#### Provision barriers

Provisions are transient tasks, so their vocabulary is `success`/`failure`
rather than the daemon's `ready`/`stopped`:

| Path | Fires when |
|---|---|
| `self.runtime.provision.<name>.scheduled` | provision is queued, deps not yet checked |
| `self.runtime.provision.<name>.running` | `cmd` has started |
| `self.runtime.provision.<name>.success` | `cmd` exited 0 (and `verify` passed if present) |
| `self.runtime.provision.<name>.done` | outcome decided — `success` *or* `failure` |

#### Cross-service barriers

Same paths but rooted at the full service identity:

```hcl
service.go.db.runtime.ready                       # wait for db to be operational
service.go.api.build.success                      # wait for api's build to finish
service.go.api.runtime.provision.migrate.success  # wait for api's migrate provision
```

The DAG resolver pulls these refs through and orders service *evaluation*
so the referenced service exists in the resolved Alphasfile by the time
alpha boots. At runtime, alpha's bringup goroutines do the actual
waiting via channels.

#### Where barriers are used

The two consumers are:

- `runtime.after = [...]` — list of barriers that must fire before the
  service's `cmd` starts.
- `provision "<n>" { after = [...] }` — same, for the provision's `cmd`.

With an empty (or absent) `after`, the entity starts immediately —
services and provisions run in parallel by default; declare deps
explicitly when ordering matters. A provision whose snippet needs its
own service operational must say so: `after = [self.runtime.ready]`.
Detached provisions (`detached = true`) start the same way but bringup
doesn't block on their `success` before declaring the stack up.

**Any provision can be invoked by another service** — `never` is not
required. A service invokes a peer's provision by pointing its own
provision's `cmd` at it (a bare reference, not a shell string):

```hcl
service "go" "kafka" {
  runtime {
    provision "create-topic" {
      after = never
      cmd   = "kafka-topics --create --topic $TOPIC"
    }
  }
}

service "go" "app" {
  runtime {
    provision "topic" {
      cmd   = service.go.kafka.runtime.provision.create-topic  # not a string
      env   = { TOPIC = "app-events" }
      after = [self.runtime.ready]  # invoke once app is up
    }
  }
}
```

The invoker runs the provider's resolved `check`/`cmd`/`verify` snippets
under its **own** barrier (`service.go.app.runtime.provision.topic@success`)
and **own** env/cwd. N consumers each get an independent run with their
own parameters. The invoking provision must not set `check`/`verify`
(the template owns them).

`after = never` on the target is **optional**. Without it, the provision
still auto-runs for its own service (immediately, or per its own
`after`) *and* is invokable by peers. `after = never` simply means
"expose this action but don't auto-run it for me" — useful when only
consumers should ever trigger it.

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
