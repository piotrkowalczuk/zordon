---
description: "Move services out of one large Alphasfile into fragment files, import them by component, and let components import each other."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/split-an-alphasfile-across-files/">https://zordon.io/how-to/split-an-alphasfile-across-files/</a></div>

# Split an Alphasfile across files

A large Alphasfile splits into fragment files that the entrypoint imports by component.
A component that depends on another imports it, so every component states what it needs.

## 1. Move a service into a component in its own file

Create `services/kafka/Alphasfile.kafka` and wrap the service in a `component` block:

```hcl
component "kafka" {
  service "go" "kafka" {
    src {
      path = "../.."               # relative to THIS file, not the entrypoint
      exe  = "./cmd/kafka"
    }
    vars = { port = net::pickport() }
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
  components = ["kafka"]
}
```

Reference the moved service as `component.kafka.service.go.kafka`, start it with `zordon start kafka/kafka`, and read it with `zordon get component.kafka.service.go.kafka.vars.port`.

## 3. Import dependencies inside the component that needs them

If a component in `services/apps/Alphasfile.apps` calls kafka, import kafka inside that component:

```hcl
component "app" {
  import "../kafka/Alphasfile.kafka" {
    components = ["kafka"]
  }

  service "go" "app" {
    src {
      path = "../.."
      exe  = "./cmd/app"
    }
    runtime {
      after = [component.kafka.service.go.kafka.runtime.ready]
    }
  }
}
```

The entrypoint then only imports `app`; kafka comes along and still loads once.
Every other component in the same file that calls kafka imports it too, because an import inside a component belongs to that component and joins the stack only with it.
To call a component declared in the same fragment, import it by the fragment's own file name, for example `import "./Alphasfile.apps" { components = ["billing"] }`.
The entrypoint cannot reference `component.kafka` until it imports kafka itself, and `zordon plan` names the missing `import` and where it goes.

## 4. Verify without starting anything

```sh
zordon plan
```

The header lists every imported file with the components taken from it, plus components that were loaded but not imported:

```
# import /repo/services/apps/Alphasfile.apps [app]
# import /repo/services/kafka/Alphasfile.kafka [kafka]
```

!!! note "Running one service on its own"
    Pick it from the main entrypoint: `zordon start kafka/kafka` starts kafka and the services it waits on, from any directory under the entrypoint.
    Do not add a second `Alphasfile` under the main entrypoint's directory to run kafka alone: walk-up turns it into a [federation](../federation.md) level, and kafka runs in both levels.

See [Imports](../alphasfile.md#imports) for every rule.
