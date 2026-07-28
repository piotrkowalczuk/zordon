---
description: "Node.js services build from a repo or local directory, infer their run command from `package.json` scripts, and take their port from env."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/nodejs/">https://zordon.io/services/nodejs/</a></div>

# Node.js services

```hcl
service "nodejs" "app" {
  src { path = "../my-node-app" }

  vars = { port = net::pickport() }
  env  = { PORT = "${self.vars.port}" }

  readiness { http { path = "/" port = self.vars.port } }
}
```

The block label is `nodejs` (matches the toolchain sub-block, 1:1). The
mise tool is `node` under the hood, but the user-facing label is
`nodejs` everywhere.

## Source

`git` (zordon bare-clones) or `src` (your local checkout). `branch` /
`tag` / `rev` pin the revision. Relative `src` resolves against the
Alphasfile's directory. **No use-only mode** (no `package` /
`cargo` / npx-style "install from registry") in v1 — Node services are
app-style, fetched from a repo or local dir.

## `exe` — project root inside the checkout

`exe` is the path within the checkout that holds `package.json`,
default `.`. Set it for monorepo subdirs (`exe = "apps/api"`). Build
and runtime both `cd` into `<checkout>/<exe>`.

## Build & run (default)

The default build is the package-manager `install`, auto-detected:

| lockfile present                | install command                     |
|---------------------------------|-------------------------------------|
| `bun.lockb` / `bun.lock`        | `bun install --frozen-lockfile`     |
| `pnpm-lock.yaml`                | `pnpm install --frozen-lockfile`    |
| `yarn.lock`                     | `yarn install --immutable` (or v1)  |
| `package-lock.json`             | `npm ci`                            |
| none, `packageManager` field    | `<that-pm> install`                 |
| none                            | `npm install`                       |

The default **does not run `build`** — install only, like Ruby's
`bundle install`. If you need `tsc` / `next build` / `vite build`,
declare it explicitly:

```hcl
build {
  cmd = ["pnpm", "run", "build"]
}
```

The default **runtime** is inferred from `package.json` — first hit:

1. `scripts.start` → `<pm> start`
2. `main` → `node <main>`
3. `bin` (string or single-entry map) → `node <bin>`
4. otherwise: hard error at runtime ("declare runtime { cmd = [...] }")

`npm start` style passes no args through by default — feed config via
env (idiomatic Node), e.g. `env = { PORT = "..." }`. For explicit argv
use `runtime { cmd = ["node", "server.js", "--port", "..."] }`.

## Corepack (pnpm / yarn)

zordon refreshes Corepack to the latest published release at toolchain
materialization (`npm install -g --prefix <state> corepack@latest`
then `corepack enable`). Bundled corepack inside older node distros
ships a stale signing-key set; without the refresh, `pnpm install`
fails with `Cannot find matching keyid`. See
`internal/tools/corepack_test.go` for the empirical matrix.

For reproducible PM version pinning, set `packageManager` in
`package.json` (Corepack's standard mechanism, e.g.
`"packageManager": "pnpm@9.12.0"`). Without that pin, Corepack uses
its baked-in default — often a too-new PM whose engine constraints
exceed the pinned Node — so pin it.

**bun** is not shimmed by Corepack. To use it, declare
`toolchain { nodejs { tools = { bun = "1.1.x" } } }` — `npm install -g`
into the pinned node's world. PM detection still picks it up either
via committed `bun.lock(b)` or a `packageManager: "bun@..."` field.

`toolchain.nodejs.tools` is the general `npm install -g` hook — use it
for any CLI you want globally available in a service's env (`pm2`,
`tsx`, `typescript`, etc.). Per-project dev-deps belong in
`devDependencies` and reach `node_modules/.bin` through the regular
install step; this `tools` block is for globals only.

## Version pin

`toolchain { nodejs { version = "22.11.0" } }` is required (no auto
fallback to host node). The version sits under
`~/.zordon/toolchain/installs/node/<version>` (mise's on-disk name is
`node`, not `nodejs`); user's host node/nvm install stays independent.

## TTY

Node services get a PTY for stdout by default —
`process.stdout` switches to block-buffered mode under a pipe, which
swallows startup `console.log` until the process exits. Override with
`log { tty = false }` if you need raw pipe behavior.
