---
description: "Run services on different versions of the same language in one stack by giving each group its own module and toolchain pin."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/pin-different-toolchain-versions/">https://zordon.io/how-to/pin-different-toolchain-versions/</a></div>

# Pin different toolchain versions per module

A top-level `toolchain { go { version = "…" } }` pins one Go for the whole file.
When one team's services still need an older Go, wrap them in a `module` with its own pin.
Everything outside the module keeps the top-level pin.

## 1. Wrap the services that need the other version

```hcl
toolchain {
  go { version = "1.27.0" }
}

module "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }

  service "go" "billing" {
    src { path = "./services/billing" }
    runtime { after = [toolchain.go.ready] }     # this module's Go
  }
}

service "go" "gateway" {
  src { path = "./services/gateway" }
  vars = { billing = module.legacy.service.go.billing.vars.port }
}
```

`toolchain.go.ready` inside `module "legacy"` waits for the module's own pin.
The same expression at top level waits for the default pin.

## 2. Address the moved services by their new names

- In expressions: `module.legacy.service.go.billing.vars.port`.
- In picks: `zordon start legacy/billing`.
- In `zordon get`: `zordon get module.legacy.service.go.billing.vars.port`.
- On disk: the binary builds to `<state>/bin/legacy/billing` (inside the module `fs::bin()` already points at `<state>/bin/legacy`, so `${fs::bin()}/billing` needs no edit) and the checkout lives under `src/legacy/billing`.

Inside the module, keep using bare `service.go.<name>` for sibling services.

## 3. Verify without starting anything

```sh
zordon plan
```

The rendered manifest shows a `toolchain { go { version = "1.22.0" } }` block nested inside `module "legacy" { … }` and `after = ["toolchain.legacy/go@ready"]` on billing.
Run `zordon start` once the plan looks right; alpha materializes both Go versions through mise and each service runs under its own.

!!! note "One pin per language per module"
    A module can pin each language once.
    A module without a pin for a language inherits the top-level pin for it.
    Two services in the same module cannot run on different versions of one language; split them into two modules.

See [Modules](../alphasfile.md#modules) for the full reference.
