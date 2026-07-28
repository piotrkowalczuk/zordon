---
title: "Rust services"
description: "Two source shapes for Rust — a crate installed from a registry or git URL, or a zordon-managed checkout built with `cargo install --path`."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/rust/">https://zordon.io/services/rust/</a></div>

# Rust services

Rust has two source shapes — a **crate** (`cargo install` from a
registry or a git URL) or a **git/src checkout** (zordon-managed
workspace, built with `cargo install --path`) — and they are mutually
exclusive.

```hcl
# crates.io registry
service "rust" "tansu" {
  crate {
    name    = "tansu"
    version = "0.6.0"
  }
  features = ["server"]
}

# cargo install --git (cargo fetches the source itself)
service "rust" "some-tool" {
  crate {
    name = "some-tool"
    git  = "https://github.com/owner/some-tool"
    tag  = "v1.0.0"          # or branch = "main", or rev = "abc1234"
  }
}

# zordon-managed workspace (clone + cargo install --path)
service "rust" "broker" {
  git {
    url = "https://github.com/acme/broker"
  }
  src { exe = "crates/broker" }   # workspace member; "" = repo root
  bin = "brokerd"           # cargo --bin target (multi-bin crate)
}

# in-place local checkout (no git clone, edit→start loop)
service "rust" "app" {
  src {
    path = "../.."
    exe = "./examples/rust/src/app"
  }
}
```

## Source blocks

- **`crate { name, version, index, registry, git, branch, tag, rev }`**
  — runs `cargo install <name>` with the cargo CLI source flags.
  `version` is mutually exclusive with `git`. `branch`/`tag`/`rev` are
  only valid with `git`. Cargo manages the fetch — no zordon workspace.
- **`git { url, branch, tag, rev }`** — a remote checkout zordon owns:
  `url` clones it, `branch`/`tag`/`rev` pin the revision. Builds via
  `cargo install --path`.
- **`src { path, exe }`** — a local checkout used in place (no clone).
  `path` points at an existing directory (relative paths resolve
  against the Alphasfile's dir); `exe` is the workspace subdir holding
  the build target (`""`/`.` = repo root). An `exe`-only `src { }` block
  rides alongside `git { }` to pick a workspace member within the clone.

Every Rust service compiles; there is no prebuilt-`$PATH` path.

## Top-level rust knobs

- **`bin`** — selects one `--bin` target from a multi-bin crate.
  Applies in both `crate` and `git` modes. The installed filename is
  the cargo bin-target name, and zordon runs `<fs::bin>/<service-name>`,
  so **name the service after the bin target** you select.
- **`features`** — passed as `--features a,b`. Applies in both modes.

## Build & run

Everything goes through `cargo install` so cargo names and places the
artifact (no guessing `target/release/...`):

```sh
# crate (immutable → no --force; reuses an already-installed binary)
CARGO_TARGET_DIR=<cache> cargo install "<name>" --root <stateDir> \
  [--version …] [--git … [--branch|--tag|--rev …]] \
  [--index …] [--registry …] [--features …] [--bin …] --locked

# git/src workspace (cwd = <checkout>/<exe>; --force so code changes are picked up)
CARGO_TARGET_DIR=<cache> cargo install --path . --root <stateDir> \
  [--features …] [--bin …] --locked --force
```

`--root <stateDir>` installs into `<stateDir>/bin`, which **is**
`fs::bin()`, so the run path `<fs::bin>/<service-name>` finds it with
no copy step. A stable `CARGO_TARGET_DIR` (under
`.zordon/cache/rust/target`) keeps compilation incremental across
runs. Override with `build { cmd = [...] }` if you need something cargo
install can't express.
