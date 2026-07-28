---
description: "Declare a `clean` snippet beside a provision's `cmd` so `zordon clean` can drop the databases, topics and files that provision created."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/clean-up-provision-side-effects/">https://zordon.io/how-to/clean-up-provision-side-effects/</a></div>

# Clean up a provision's side effects

A provision's `cmd` often creates state that outlives the stack: a database, a Kafka topic, generated files, a marker.
Declare a `clean` snippet next to it and `zordon clean` will run it to undo that state.

## Declare the teardown

Add `clean` to the provision, alongside `cmd`.
It is a shell snippet interpolated in the same scope as `cmd` (it can read `self.vars`, `${fs::state()}`, and the service's interpolation functions).

```hcl
service "pkg" "postgres" {
  package = "asdf:mise-plugins/mise-postgres@16.4"
  vars    = { port = net::pickport(), data = "${fs::tmp()}/pgdata" }
  runtime {
    after = [self.runtime.provision.create-db.success]
    provision "create-db" {
      cmd   = "createdb -h 127.0.0.1 -p ${self.vars.port} -U postgres app"
      clean = "dropdb -h 127.0.0.1 -p ${self.vars.port} -U postgres --if-exists app"
    }
    cmd = ["postgres", "-D", "${self.vars.data}", "-p", "${self.vars.port}", "-k", "${self.vars.data}"]
  }
  readiness { tcp { port = self.vars.port } }
}
```

## Run it

`zordon clean` operates on a **stopped** stack, so stop first:

```bash
zordon stop
zordon clean
```

Or do both in one step:

```bash
zordon stop --clean
```

`zordon clean` spawns a transient supervisor that rebuilds each service's env, toolchain, and cwd — but does not start the services — then runs every `clean` snippet in reverse declaration order.

## Notes

A plain `zordon stop` never runs `clean` snippets; teardown is always explicit.

Provisions without a `clean` snippet are skipped, so adding `clean` to a few provisions is safe.

A still-running stack is refused — run `zordon stop` first, or use `zordon stop --clean`.

Only the invocation level is cleaned; shared parent levels in a [federation](../federation.md) chain are left untouched, the same scope as `zordon stop`.

A clean snippet that needs a binary from its own package or toolchain reaches it the same way a `cmd` does — see [Use a binary from another service or toolchain](use-a-binary-from-another-service.md).
