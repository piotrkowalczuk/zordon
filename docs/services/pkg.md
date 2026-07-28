---
description: "Run an off-the-shelf native binary — redis, postgres, etcd — installed by mise and supervised as an ordinary child process, with no container."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/pkg/">https://zordon.io/services/pkg/</a></div>

# Package (`pkg`) services

```hcl
service "pkg" "redis" {
  package = { name = "redis", version = "7.4.1" }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["redis-server", "--port", "${self.vars.port}"]
  }

  readiness {
    tcp { port = self.vars.port }
  }
}
```

A `pkg` service runs a **native package** — a database, broker, or other
off-the-shelf binary (redis, postgres, etcd, …) — installed by
[mise](https://mise.jdx.dev) and supervised like any other zordon
service. There is **no container and no daemon**: the binary is an
ordinary child of the supervisor, so the same `tommy` + process-group +
registry machinery reaps it. No orphan window, no engine-specific flags.

## Coordinate

`package` is a mise coordinate. It is **required**, and so is a version.
Two forms:

| form | example |
|------|---------|
| object | `package = { name = "redis", version = "7.4.1" }` |
| object + backend | `package = { name = "etcd-io/etcd", backend = "aqua", version = "3.5.17" }` |
| string | `package = "redis@7.4.1"` |
| string + backend | `package = "aqua:etcd-io/etcd@3.5.17"` |

`name` is explicit (it is **not** derived from the service label, so a
service `"cache"` can run package `redis`).

### Backend resolution

A **bare name** is resolved through mise's registry, which picks the
backend from an ordered list — first installable wins (`redis →
vfox→asdf`, `etcd → aqua→asdf→vfox`). zordon adds no backend logic; it
passes the name to mise exactly like `mise use redis`.

To **pin a backend** (e.g. to get a prebuilt binary instead of a
source build), set `backend` in the object or use the `backend:name`
string form: `aqua:`, `ubi:`, `asdf:`, `vfox:`, `cargo:`, `npm:`, …

> First start may **build from source** (vfox/asdf backends compile
> redis/postgres). Prefer a prebuilt backend (`aqua:`, `ubi:`) where one
> exists to avoid the compile.

## Run command

`runtime { cmd = [...] }` is **required** — packages have no inferable
entrypoint. It runs under the package's mise environment, so the
package's binaries (`redis-server`, `postgres`, `initdb`, `etcdctl`, …)
are on `PATH`.

`src` / `git` / `crate` / `package`-string-for-go / `features` / `bin`
are **not** allowed on a pkg service — there is no source build.

## Build env (`build { env }`) — source-compiled packages

A backend that compiles from source (vfox/asdf for redis/postgres) may
need configure flags or build vars. A pkg service's `build { env }` is
injected straight into `mise install`:

```hcl
service "pkg" "postgres" {
  # asdf backend + --without-icu so postgres builds without a system
  # pkg-config / ICU (the default vfox backend forces ICU).
  package = "asdf:mise-plugins/mise-postgres@16.4"

  build {
    env = { POSTGRES_EXTRA_CONFIGURE_OPTIONS = "--without-icu" }
  }
  # ...
}
```

`build { env }` overlays only the **install** environment, not the run
environment. (The install also inherits the host env, so a value set in
your shell — `PKG_CONFIG_PATH`, etc. — flows through too.)

## Readiness — TCP

Databases and brokers don't speak HTTP, so readiness uses a **TCP
connect** probe: a successful dial to the port means the listener is
accepting. Same k8s-style knobs (`period`, `timeout`,
`failure_threshold`, …) as the HTTP probe; exactly one of `http {}` /
`tcp {}` per `readiness` block.

```hcl
readiness {
  tcp { port = self.vars.port }
  period            = "200ms"
  failure_threshold = 50
}
```

## One-time init (e.g. postgres `initdb`)

Use a `provision` and gate the service on it with `runtime.after`, so
the package's setup runs once before the daemon starts:

```hcl
service "pkg" "postgres" {
  package = "asdf:mise-plugins/mise-postgres@16.4"
  build { env = { POSTGRES_EXTRA_CONFIGURE_OPTIONS = "--without-icu" } }

  vars = { port = net::pickport(), data = "${fs::tmp()}/pgdata" }

  runtime {
    after = [self.runtime.provision.initdb.success]

    provision "initdb" {
      check = "test -f ${self.vars.data}/PG_VERSION"
      cmd   = "initdb -D ${self.vars.data} -U postgres --auth=trust"
    }

    cmd = ["postgres", "-D", "${self.vars.data}", "-p", "${self.vars.port}", "-k", "${self.vars.data}"]
  }

  readiness {
    tcp { port = self.vars.port }
  }
}
```

The provision runs under the same package environment, so `initdb` /
`psql` resolve without any extra wiring.

## Lifecycle

Because the package is a real child process (not a container under a
daemon), zordon's standard supervision applies unchanged: a group
SIGTERM→SIGKILL on stop, `tommy` reaping it if alpha is SIGKILLed, and
the registry reaper cleaning up after an unclean shutdown.

See `examples/pkg/Alphasfile` for redis + postgres.

## Standalone CLI tools (`toolchain { pkg { tools } }`)

A `pkg` **service** runs a package as a supervised process. To install a
package purely for its **command-line binary** — a tool like `atlas`,
`sqlc`, or `golang-migrate` that no service supervises and that belongs
to no language runtime — declare it under `toolchain { pkg { tools } }`
instead:

```hcl
toolchain {
  pkg {
    # key = mise ref (optionally backend-qualified), value = version.
    tools = {
      "aqua:ariga/atlas" = "1.2.0"
    }
  }
}
```

Each entry is installed with `mise install <ref>@<version>` (the same
mechanism, backends, and cache as a pkg service's `package`). The map is
**ref → version**; the ref carries any backend qualifier, so there is no
separate `backend`/object form here. A `version` is **required** for each
entry, and the block must declare at least one tool.

The installed binaries are **not** put on `PATH` globally. A build,
runtime, or provision step opts in with
`fs::toolchain::bin(toolchain.pkg)` — the dir holding every tool declared
in the block — layered onto `PATH` with `env::prepend`:

```hcl
service "go" "app" {
  # ...
  runtime {
    after = [self.runtime.provision.migrate.success]

    provision "migrate" {
      env = { PATH = env::prepend(fs::toolchain::bin(toolchain.pkg)) }
      cmd = "atlas migrate apply --url sqlite://${self.vars.db} --dir file://migrations"
    }
  }
}
```

`fs::toolchain::bin(toolchain.pkg)` blocks until the tools are installed,
so a step that uses one is automatically gated on the install — the same
deferred-resolution model as
[`fs::service::bin` / `fs::toolchain::bin` for a language toolchain](../how-to/use-a-binary-from-another-service.md).

See `examples/pkg_tools/Alphasfile` for `atlas` running a SQLite
migration, and the [how-to guide](../how-to/install-a-cli-tool-with-mise.md).
