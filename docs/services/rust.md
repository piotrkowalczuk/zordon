# Rust services

Rust has two source shapes — a **crate** from crates.io, or a
**git/src** checkout — and they are mutually exclusive.

```hcl
# crates.io
service "rust" "tansu" {
  crate    = "tansu"
  features = ["server"]
}

# from a repo / local checkout
service "rust" "broker" {
  git = "https://github.com/acme/broker"
  exe = "crates/broker"   # workspace member; "" = repo root
  bin = "brokerd"         # cargo --bin target (multi-bin crate)
}
```

## Source

- **`crate`** — a crates.io crate. Mutually exclusive with `git`/`src`
  (declaring both is an error).
- **`git`/`src`** — a checkout, like Go. `branch`/`tag`/`rev` pin it.

Every Rust service compiles; there is no prebuilt-`$PATH` path.

## `exe` vs `bin`

- **`exe`** — for `git`/`src`, the **workspace subdir** holding the
  crate to build (`""`/`.` = repo root). It's a path, never a binary
  name.
- **`bin`** — selects one `--bin` target from a multi-bin crate. Its
  installed filename is the cargo bin-target name, and zordon runs
  `<fs::bin>/<service-name>`, so **name the service after the bin
  target** you select.
- **`features`** — passed as `--features a,b`.

## Build & run

Everything goes through `cargo install` so cargo names and places the
artifact (no guessing `target/release/...`):

```sh
# crate (immutable → no --force; reuses an already-installed binary)
CARGO_TARGET_DIR=<cache> cargo install "<crate>" --root <stateDir> \
  [--features …] [--bin …] --locked

# git/src (cwd = checkout; --force so code changes are picked up)
CARGO_TARGET_DIR=<cache> cargo install --path "<exe|.>" --root <stateDir> \
  [--features …] [--bin …] --locked --force
```

`--root <stateDir>` installs into `<stateDir>/bin`, which **is**
`fs::bin()`, so the run path `<fs::bin>/<service-name>` finds it with
no copy step. A stable `CARGO_TARGET_DIR` (under
`.zordon/cache/rust/target`) keeps compilation incremental across
runs. Override with `build = "..."` if you need something cargo
install can't express.
