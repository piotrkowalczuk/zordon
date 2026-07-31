---
description: "Reach a binary shipped by a different service's package or another toolchain using fs::service::bin and env::prepend."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/use-a-binary-from-another-service/">https://zordon.io/how-to/use-a-binary-from-another-service/</a></div>

# Use a binary from another service or toolchain

A provision runs with only its own service's toolchain bin dir on `PATH`.
It cannot find a binary that ships with a *different* service's package or a *different* toolchain's tool world.
Two functions surface those bin dirs, and `env::prepend` puts them on `PATH`.

These resolve at provision-run time, so the referenced toolchain must be installed first — gate on it with `after`.

!!! note "`fs::bin()` is something else"
    `fs::bin()` is the per-invocation build-output dir (`<state>/bin`), where zordon writes artifacts it compiles from source.
    It is not where a mise-installed package's executables live.

## From another service: `fs::service::bin`

Use `fs::service::bin(<service-ref>)` when the binary ships with another [`pkg` service](../services/pkg.md)'s package — for example `pg_dump`, built together with postgres.

```hcl
service "pkg" "postgres" {
  package = "asdf:mise-plugins/mise-postgres@16.4"
  vars    = { port = net::pickport(), data = "${fs::tmp()}/pgdata" }
  runtime {
    after = [self.runtime.provision.initdb.success]
    provision "initdb" {
      check = "test -f ${self.vars.data}/PG_VERSION"
      cmd   = "initdb -D ${self.vars.data} -U postgres --auth=trust"
    }
    cmd = ["postgres", "-D", "${self.vars.data}", "-p", "${self.vars.port}", "-k", "${self.vars.data}"]
  }
  readiness { tcp { port = self.vars.port } }
}

service "go" "app" {
  vars = { port = net::pickport(), out = "${fs::state()}/dump.sql" }
  runtime {
    after = [self.runtime.provision.dump.success]
    provision "dump" {
      after = [service.pkg.postgres.runtime.ready]
      env   = { PATH = env::prepend(fs::service::bin(service.pkg.postgres)) }
      cmd   = "pg_dump -h 127.0.0.1 -p ${service.pkg.postgres.vars.port} -U postgres postgres > ${self.vars.out}"
    }
    cmd = ["${fs::bin()}/app", "-addr", "127.0.0.1:${self.vars.port}"]
  }
}
```

The `after` gate guarantees postgres is installed and accepting connections before `dump` runs.
`env::prepend(fs::service::bin(...))` puts postgres's bin dir at the front of the provision's `PATH`, so the bare `pg_dump` resolves to it.

For a `pkg` service the *service* is the only handle to its toolchain, so `fs::service::bin` is the way to reach its binaries.

## From a toolchain: `fs::toolchain::bin`

Use `fs::toolchain::bin(toolchain.<lang>)` when the binary is a tool installed into a language toolchain's world — for example a Go tool you want in a Ruby project.
There may be no service for that toolchain to reference.

```hcl
toolchain {
  go {
    version = "1.26.4"
    tools   = { "github.com/example/jsonfmt" = "latest" } # go install
  }
  ruby { version = "3.3.10" }
}

service "ruby" "api" {
  git { url = "github.com/acme/api" }
  runtime {
    provision "format" {
      env = { PATH = env::prepend(fs::toolchain::bin(toolchain.go)) }
      cmd = "jsonfmt ./config/*.json"
    }
    cmd = ["ruby", "app.rb"]
  }
}
```

`fs::toolchain::bin(toolchain.go)` resolves to the Go toolchain's bin dir, which holds `go install`-ed tools, so the Ruby provision finds `jsonfmt`.

## Use the path directly

Both functions also expand inside a snippet, when you want the full path rather than touching `PATH`:

```hcl
cmd = "${fs::service::bin(service.pkg.postgres)}/pg_dump --version"
```

## What happens with a bad reference

A wrong-typed argument (`fs::service::bin("redis")`), a missing service or toolchain (`service.pkg.ghost`, `toolchain.ghost`), or a typo in the function name is a configuration error.
It is reported at eval with the file location, and `zordon start` aborts before anything runs.
If a referenced toolchain is valid but its install fails at runtime, the dir is simply not added and the provision's command fails with the shell's own "command not found".
