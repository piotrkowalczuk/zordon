---
description: "Declare a standalone CLI such as atlas under the toolchain pkg tools block, put it on PATH, and drive it from a provision."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/install-a-cli-tool-with-mise/">https://zordon.io/how-to/install-a-cli-tool-with-mise/</a></div>

# Install a CLI tool from a mise package (e.g. atlas)

You need a command-line tool — `atlas`, `sqlc`, `golang-migrate`, `dbmate` — during setup, but it is neither a service you supervise nor a tool of some language runtime.
Declare it under `toolchain { pkg { tools } }`: zordon installs it with mise and hands it to the steps that ask for it.

This guide runs an [`atlas`](https://atlasgo.io) schema migration against a local SQLite file from a provision.

## 1. Declare the tool

The map key is the mise ref (optionally backend-qualified), the value is the version.

```hcl
toolchain {
  pkg {
    tools = {
      "aqua:ariga/atlas" = "1.2.0"
    }
  }
}
```

`aqua:` pins the aqua backend, which ships atlas as a prebuilt binary — no source compile.
A bare `atlas` would let mise's registry pick a backend; see [backend resolution](../services/pkg.md#backend-resolution).

## 2. Put it on PATH where you use it

The tool is not on `PATH` globally.
A step opts in with `fs::toolchain::bin(toolchain.pkg)` — the dir holding every tool in the block — layered onto `PATH` with `env::prepend`.

```hcl
service "go" "app" {
  src { path = "." }

  vars = {
    port = net::pickport()
    db   = "${fs::state()}/app.db"
  }

  runtime {
    # The app starts only after the schema is migrated.
    after = [self.runtime.provision.migrate.success]

    provision "migrate" {
      env    = { PATH = env::prepend(fs::toolchain::bin(toolchain.pkg)) }
      cmd    = "atlas migrate apply --url sqlite://${self.vars.db}?_fk=1 --dir file://${fs::src()}/migrations"
      verify = "atlas migrate status --url sqlite://${self.vars.db}?_fk=1 --dir file://${fs::src()}/migrations | grep -q 'Migration Status: OK'"
    }

    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness { http { path = "/", port = self.vars.port } }
}
```

`fs::toolchain::bin(toolchain.pkg)` blocks until atlas is installed, so the provision is gated on the install automatically.
`atlas migrate apply` is idempotent, so a restart re-runs it as a no-op.

## 3. Provide the migrations

atlas reads versioned migration files plus an `atlas.sum` integrity file.
Keep them in your repo (here under `migrations/`) and regenerate the checksum whenever the SQL changes:

```sh
atlas migrate hash --dir file://migrations
```

`migrations/` is referenced through `fs::src()`, so it must be **committed** — a service runs from a git worktree checked out at `HEAD`, which does not include uncommitted files.

## 4. Run it

```sh
zordon start
```

First start downloads atlas via mise (cached under `~/.zordon` afterwards), runs the migration, then brings the app up.

## Notes

- **More than one tool** — add more entries; their bins are pooled behind the same `fs::toolchain::bin(toolchain.pkg)`.
- **Use it in a build, not a provision** — `fs::toolchain::bin(toolchain.pkg)` works in any `env {}` (build / runtime / provision).
- **A tool that ships with a service instead** — reach it with [`fs::service::bin`](use-a-binary-from-another-service.md).

See `examples/pkg_tools/Alphasfile` for the complete, runnable version.
