---
description: "Run services on different versions of the same language in one stack by giving each group its own component and toolchain pin."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/pin-different-toolchain-versions/">https://zordon.io/how-to/pin-different-toolchain-versions/</a></div>

# Pin different toolchain versions per component

A top-level `toolchain { go { version = "…" } }` pins one Go for the whole file.
When one team's services still need an older Go, wrap them in a `component` with its own pin.
Everything outside the component keeps the top-level pin.

## 1. Wrap the services that need the other version

```hcl
toolchain {
  go { version = "1.27.0" }
}

component "legacy" {
  toolchain {
    go { version = "1.22.0" }
  }

  service "go" "billing" {
    src { path = "./services/billing" }
    vars    = { port = net::pickport() }
    runtime { after = [toolchain.go.ready] }     # this component's Go
  }
}

service "go" "gateway" {
  src { path = "./services/gateway" }
  vars = { billing = component.legacy.service.go.billing.vars.port }
}
```

`toolchain.go.ready` inside `component "legacy"` waits for the component's own pin.
The same expression at top level waits for the default pin.

## 2. Address the moved services by their new names

- In expressions: `component.legacy.service.go.billing.vars.port`.
- In picks: `zordon start legacy/billing`, `zordon start billing` while no other service is named billing, or `zordon start legacy` for the whole component.
- In `zordon get`: `zordon get component.legacy.service.go.billing.vars.port`.
- On disk: the binary builds to `<state>/bin/legacy/billing` (inside the component `fs::bin()` already points at `<state>/bin/legacy`, so `${fs::bin()}/billing` needs no edit) and a checkout, when the service has one (a git source, or a workspace that owns it), lives under `src/legacy/billing`.

Inside the component, keep using bare `service.go.<name>` for sibling services.

## 3. Verify without starting anything

```sh
zordon plan
```

The rendered manifest shows a `toolchain { go { version = "1.22.0" } }` block nested inside `component "legacy" { … }` and `after = ["component.legacy.toolchain.go@ready"]` on billing.
Run `zordon start` once the plan looks right; alpha materializes both Go versions through mise and each service runs under its own.

!!! note "One pin per language per component"
    A component can pin each language once.
    A component without a pin for a language inherits the top-level pin for it.
    Two services in the same component cannot run on different versions of one language; split them into two components.

See [Components](../alphasfile.md#components) for the full reference.
