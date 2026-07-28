---
description: "What zordon, alpha and tommy each do, how they resolve one another through PATH, and why all three must share a single directory."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/reference/binaries/">https://zordon.io/reference/binaries/</a></div>

# Binaries and layout

A zordon installation is three executables.
All three must sit in one directory, and that directory must be on your `$PATH`.

| Binary | Role | Required |
|---|---|---|
| `zordon` | The CLI you invoke. Resolves the Alphasfile and drives a supervisor. | Yes |
| `alpha` | The supervisor. One per chain level, long-lived, owns the control socket and the service processes. | Yes |
| `tommy` | A reaper wrapper `alpha` interposes in front of every service. | Strongly recommended |

## How they find each other

The two hops resolve differently, and the difference is what dictates the layout.

### `zordon` → `alpha`: `$PATH`

`zordon` spawns `alpha` by name, so the lookup goes through `$PATH`.

Override with `--alpha` (accepts a name or an absolute path) or the `ZORDON_ALPHA` environment variable.

If `alpha` is not on `$PATH`, `zordon start` fails.

### `alpha` → `tommy`: sibling only

`alpha` looks for `tommy` in exactly two places, in order:

1. The `ZORDON_TOMMY_BIN` environment variable, or the `--tommy-bin` flag.
2. A `tommy` sitting next to `alpha`'s own executable.

There is deliberately **no `$PATH` fallback**.
`tommy` is exec'd ahead of every service, so a writable directory earlier on `$PATH` could otherwise substitute a malicious `tommy` into every spawn.

If `tommy` cannot be found, bringup still proceeds and `alpha` logs an error.
Services then run unwrapped, which means an `alpha` killed by `SIGKILL` or the OOM killer will orphan them instead of taking them down.

## Consequences for installation

Every supported install puts all three in one directory:

- Homebrew places them together under `$(brew --prefix)/bin`.
- A release tarball holds all three at its root; extract them into a single directory on your `$PATH`.
- `go install github.com/piotrkowalczuk/zordon/cmd/...@latest` puts them in a shared `$GOBIN`.
- An [MCP bundle](../how-to/install-the-mcp-bundle.md) (`.mcpb`) carries all three internally and launches `zordon mcp` for a client, so nothing needs to be on `$PATH`.

Mixing sources is the one thing to avoid.
A `zordon` from Homebrew with a stale `alpha` still earlier on `$PATH` from an old `go install` is the common case; `zordon` warns when the pair does not match.

## Version

Each binary prints its build identity and exits:

```sh
zordon --version
alpha --version
tommy --version
```

`zordon` also accepts `-V` and `version`.

The version comes from the release tag when installed from a release, and from Go's embedded build info otherwise — the module version for `go install ...@version`, the VCS revision for a local build, suffixed `+dirty` when the working tree was modified.

## Supported platforms

| OS | Architectures |
|---|---|
| macOS | `arm64`, `amd64` |
| Linux | `arm64`, `amd64` |

Windows is not supported.
zordon relies on POSIX process groups, signals and `flock`, which have no direct equivalents there.

Release binaries are built with `CGO_ENABLED=0`, so macOS builds use Go's pure-Go DNS resolver.
Readiness probes target loopback addresses, so this has no practical effect.
