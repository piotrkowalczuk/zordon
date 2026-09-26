---
description: "Move services out of one large Alphasfile into fragment files, import them by module, and let modules require each other."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/split-an-alphasfile-across-files/">https://zordon.io/how-to/split-an-alphasfile-across-files/</a></div>

# Split an Alphasfile across files

A large Alphasfile splits into fragment files that the entrypoint imports by module.
A module that depends on another requires it, so every module states what it needs.

## 1. Move a service into a module in its own file

Create `services/kafka/Alphasfile.kafka` and wrap the service in a `module` block:

```hcl
module "kafka" {
  service "go" "kafka" {
    src {
      path = "../../.."            # relative to THIS file, not the entrypoint
      exe  = "./cmd/kafka"
    }
    runtime {
      cmd = ["${fs::bin()}/${self.name}"]
    }
  }
}
```

Name the file anything except `Alphasfile`: a file with that exact name is an entrypoint and cannot be imported.
Recompute relative `src { path }` values, because they now resolve against the fragment's directory.

## 2. Import it where it is used

In the entrypoint:

```hcl
import "./services/kafka/Alphasfile.kafka" {
  modules = ["kafka"]
}
```

Reference the moved service as `module.kafka.service.go.kafka`, start it with `zordon start kafka/kafka`, and read it with `zordon get module.kafka.service.go.kafka.vars.port`.

## 3. Require dependencies inside the module that needs them

If a module in `services/apps/Alphasfile.apps` calls kafka, require kafka inside that module:

```hcl
module "app" {
  require "../kafka/Alphasfile.kafka" {
    modules = ["kafka"]
  }

  service "go" "app" {
    runtime {
      after = [module.kafka.service.go.kafka.runtime.ready]
    }
  }
}
```

The entrypoint then only imports `app`; kafka comes along and still loads once.
Every other module in the same file that calls kafka requires it too, because a `require` belongs to one module and joins the stack only with it.
To call a module declared in the same fragment, require it by the fragment's own file name, for example `require "./Alphasfile.apps" { modules = ["billing"] }`.
The entrypoint cannot reference `module.kafka` until it imports kafka itself, and `zordon plan` names the missing `import` or `require` and where it goes.

## 4. Verify without starting anything

```sh
zordon plan
```

The header lists every imported file with the modules taken from it, plus modules that were loaded but not imported:

```
# import /repo/services/kafka/Alphasfile.kafka [kafka]
# import /repo/services/apps/Alphasfile.apps [app]
```

!!! note "Running one service on its own"
    Pick it from the main entrypoint: `zordon start kafka/kafka` starts kafka and the services it waits on, from any directory under the entrypoint.
    Do not add a second `Alphasfile` under the main entrypoint's directory to run kafka alone: walk-up turns it into a [federation](../federation.md) level, and kafka runs in both levels.

See [Imports](../alphasfile.md#imports) for every rule.
