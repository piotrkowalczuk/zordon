# Go services

```hcl
service "go" "prometheus" {
  git = "github.com/prometheus/prometheus"
  tag = "v3.11.3"
  exe = "./cmd/prometheus"   # main package, relative to the repo root
  doubleDash = true
  arguments = { "config.file" = "prometheus.yml" }
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

The default build, run with cwd = the per-invocation checkout:

```sh
go build -o "<fs::bin>/<service-name>" <exe|.>
```

The artifact lands in `fs::bin()` — outside the source checkout, so a
`src` primary's working tree is never dirtied. With no `cmd`, zordon
runs `<fs::bin>/<service-name>` (cwd = checkout, so relative paths like
`config.file = "prometheus.yml"` resolve against the source). Set
`cmd = [...]` only when you need an explicit argv (subcommands, custom
flags); reference the binary as `${fs::bin()}/<name>`.

Override the whole step with `build = "..."` (interpolated; runs with
cwd = checkout) if the default doesn't fit (codegen, ldflags, etc.):

```hcl
build = "go build -ldflags \"-X main.Tag=${pathhash()}\" -o ${fs::bin()}/app ./cmd/app"
```
