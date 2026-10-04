---
description: "Move services out of one large Alphasfile into Alphasfile.<name> parts next to it, and into packages when they belong in another directory."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/split-an-alphasfile-across-files/">https://zordon.io/how-to/split-an-alphasfile-across-files/</a></div>

# Split an Alphasfile across files

A large Alphasfile splits into parts: files named `Alphasfile.<name>` next to it.
zordon reads every part of a directory together with its `Alphasfile`, so nothing has to import them.

## 1. Move a component into a part

Create `Alphasfile.kafka` next to the `Alphasfile` and move the component into it:

```hcl
component "kafka" {
  service "go" "kafka" {
    src {
      path = "."
      exe  = "./cmd/kafka"
    }
    vars = { port = net::pickport() }
    runtime {
      cmd = ["${fs::bin()}/${self.name}"]
    }
  }
}
```

Keep the part in the same directory as the `Alphasfile`: a file in a subdirectory is not a part, and a directory without an `Alphasfile` is never read.
Relative `src { path }` values stay the same, because a part lives in the same directory.

## 2. Reference it from any other part

Components of all parts see each other, so the `Alphasfile` keeps using `component.kafka.service.go.kafka` with no import:

```hcl
component "app" {
  service "go" "app" {
    runtime {
      after = [component.kafka.service.go.kafka.runtime.ready]
    }
  }
}
```

Move `env`, `dotenv` or `sysenv` into a part if it belongs there; the unit adds them up, and a key set in two parts is an error.

## 3. Split a package the same way

In a package's directory, every part opens the same package:

```hcl
# shop/Alphasfile.admin
package "shop" {
  component "admin" {
    service "go" "panel" { … }
  }
}
```

A part may also declare features, inputs and outputs; they join the package's API, and a name declared in two parts is an error.

## 4. Move a shared piece into its own directory as a package

When two stacks need the same components, give them a directory of their own with an `Alphasfile` holding a `package` block, expose what others need as outputs, and import the directory:

```hcl
import "./services/kafka" {}
```

## 5. Verify without starting anything

```sh
zordon plan
```

Every component of every part renders under the level.

See [Parts](../alphasfile.md#parts) for every rule and [examples/fragments](https://github.com/piotrkowalczuk/zordon/tree/main/examples/fragments) for a runnable stack.
