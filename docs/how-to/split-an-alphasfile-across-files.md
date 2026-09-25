---
description: "Move services out of one large Alphasfile into fragment files, import them by module, and let fragments import each other."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/split-an-alphasfile-across-files/">https://zordon.io/how-to/split-an-alphasfile-across-files/</a></div>

# Split an Alphasfile across files

A large Alphasfile splits into fragment files that the entrypoint imports by module.
A fragment that depends on another imports it too, so every file states what it needs.

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
import "services/kafka/Alphasfile.kafka" {
  modules = ["kafka"]
}
```

Reference the moved service as `module.kafka.service.go.kafka`, start it with `zordon start kafka/kafka`, and read it with `zordon get module.kafka.service.go.kafka.vars.port`.

## 3. Let dependent fragments import their dependencies

If `services/apps/Alphasfile.apps` holds modules that call kafka, import kafka from that file:

```hcl
import "../kafka/Alphasfile.kafka" {
  modules = ["kafka"]
}

module "app" {
  service "go" "app" {
    runtime {
      after = [module.kafka.service.go.kafka.runtime.ready]
    }
  }
}
```

The entrypoint then only imports `app`; kafka comes along and still loads once.
The entrypoint cannot reference `module.kafka` until it imports kafka itself, and `zordon plan` names the missing import if it tries.

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
    Put a small entrypoint next to the fragment that imports it, for example `services/kafka/dev/Alphasfile`.
    Keep it outside the directories the main entrypoint walks up from, or it becomes a [federation](../federation.md) level and kafka runs twice.

See [Imports](../alphasfile.md#imports) for every rule.
