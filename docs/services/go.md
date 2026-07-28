---
title: "Define a Go service from git or a checkout"
description: "Source a Go service from git or a local checkout, point `exe` at the main package, and let zordon build and supervise the binary."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/go/">https://zordon.io/services/go/</a></div>

# Define a Go service from git or a checkout

```hcl
service "go" "prometheus" {
  git {
    url = "github.com/prometheus/prometheus"
    tag = "v3.11.3"
  }
  src { exe = "./cmd/prometheus" }   # main package, relative to the repo root
  arguments {
    values = { main = { "config.file" = "prometheus.yml" } }
    options { prefix = "--" }
  }
  readiness { http { path = "/-/ready" port = 9020 } }
}
```

## Source

`git` (zordon bare-clones) or `src` (your local checkout). `branch` /
`tag` / `rev` pin the revision. Relative `src` resolves against the
Alphasfile's directory. There is no `crate` for Go.

## `exe` — the build target

`exe` is the **main package path, relative to the primary root**
(the `src` dir, or the git-clone root). Default `.` (main at repo
root). Set it when the binary lives elsewhere, e.g. `./cmd/foo`.
`exe` never names a finished binary.

## Build & run

The default build runs with cwd = the service's working dir
(`<checkout>/<exe>`, the exe-anchor — the checkout root when `exe` is
unset or `.`), building the package there:

```sh
go build -o "<fs::bin>/<service-name>" .
```

The artifact lands in `fs::bin()` — outside the source checkout, so a
`src` primary's working tree is never dirtied. With no `runtime { cmd }`,
zordon runs `<fs::bin>/<service-name>` from that same working dir, so
relative paths in flags (e.g. `config.file = "prometheus.yml"`) resolve
against `<checkout>/<exe>`. Set `runtime { cmd = [...] }` only when you
need an explicit argv (subcommands, custom flags); reference the binary
as `${fs::bin()}/<name>`.

Override the whole step with `build { cmd = [...] }` (argv, interpolated,
same `<checkout>/<exe>` cwd) if the default doesn't fit (codegen,
ldflags, etc.) — wrap in `sh -lc` when you need shell expansion:

```hcl
build {
  cmd = ["go", "build", "-ldflags", "-X main.Tag=${src::hash()}", "-o", "${fs::bin()}/app", "./cmd/app"]
}
```
