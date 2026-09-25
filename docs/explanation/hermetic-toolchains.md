---
description: "Why every toolchain install and every service runs under the same closed world, what zordon relocates out of HOME per language, and what it deliberately leaves alone."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/explanation/hermetic-toolchains/">https://zordon.io/explanation/hermetic-toolchains/</a></div>

# Hermetic toolchains

zordon pins a language toolchain by version, but a version pin alone does not make a build reproducible.
Language tooling reads configuration and credentials from the developer's home directory, and it consults environment variables the developer's shell happens to export.
Two machines with the same Alphasfile can therefore build the same service differently, and a machine that "works" often works only because of a file a fresh clone does not have.
This page explains how zordon closes that gap and where the boundary of that closure lies.

## One closed world for services and installs

An Alphasfile's `sysenv` is the complete list of host variables a spawned process may inherit.
That was always true for services, builds and provisions.
It is now equally true for the installs that materialize a toolchain: every `mise` invocation, and every installer it runs (`gem install`, `npm install -g`, `go install`, `cargo install`, `mise install`), sees the sysenv-filtered environment plus zordon's own pins.
A `GEM_HOME`, `NPM_CONFIG_REGISTRY`, `GOFLAGS` or `CARGO_HOME` exported by the shell cannot decide where a declared tool lands or which registry it comes from, because it never reaches the installer.

`PATH` is one deliberate exception.
When `sysenv` does not declare it, mise still needs to find `git`, `curl` and `tar`, so the host `PATH` is used for the install.
Declaring `PATH` in `sysenv` makes that explicit and is what every shipped example does.
The other is mise's own GitHub credential: `MISE_GITHUB_TOKEN` (or `GITHUB_TOKEN`) from the environment zordon was started in is handed to mise, because ruby, rust and aqua releases are resolved through the GitHub API and its anonymous limit is exhausted within minutes on shared CI runners.
It reaches mise only, never a service, so it is the installer's credential rather than a hole in the closed world.

## Relocating what lives in HOME

Passing `HOME` through `sysenv` is unavoidable: the tools need one.
But almost every language toolchain keeps machine-local configuration and caches under it, and those files are exactly the "works on my machine" state the closed world is meant to exclude.
Rather than hide `HOME`, zordon points each tool's own override at a directory it owns under the toolchain data dir (`~/.zordon/toolchain` by default), next to the mise installs.

| toolchain | ambient source under HOME | zordon-owned replacement |
|---|---|---|
| go | `~/.config/go/env` (the `go env -w` file), `~/.netrc`, the build cache | `GOENV=off`, `NETRC` and `GOCACHE` under `go-home/`; `GOTOOLCHAIN=local` keeps the pin |
| rust | `~/.cargo` (config, credentials, installed binaries), `~/.rustup` | mise's cargo and rustup homes under `cargo-home/` and `rustup-home/` |
| nodejs | `~/.npmrc`, `~/.npm`, corepack, yarn 1, pnpm and bun caches | `NPM_CONFIG_USERCONFIG`, `NPM_CONFIG_CACHE`, `COREPACK_HOME`, `YARN_CACHE_FOLDER`, the pnpm store and the bun cache under `node-home/` |
| java | `~/.m2` (settings, repository, wrapper distributions), `~/.mavenrc`, `~/.gradle` | `MAVEN_USER_HOME`, an empty user `settings.xml` and `maven.repo.local` under `java-home/m2/`, `MAVEN_SKIP_RC`, `GRADLE_USER_HOME` under `java-home/gradle/` |
| ruby | `~/.gem/ruby/<abi>`, `~/.gemrc`, `~/.bundle` | `GEM_HOME`/`GEM_PATH` pinned to the interpreter's gem dir, `gem install --norc`, `BUNDLE_USER_HOME` under `ruby-home/<version>/`, `BUNDLER_VERSION` from the declared tool |
| pkg | nothing beyond mise itself | — |

Two consequences follow.
The relocated caches are shared by every project on the machine, like the mise installs they sit beside, so a warm cache stays warm across checkouts and workspaces.
And the tool installs run under the same relocations as the services, so what `gem install` or `go install` produces is what the runtime finds.

## Precedence

The relocations are defaults, not overrides.
They sit in the toolchain tier of the environment, above mise's own output and below `toolchain { <lang> { env } }`, the file-level `dotenv`/`env`, and a service's own `dotenv`/`env`.
A project that legitimately needs `GOTOOLCHAIN=auto`, a private npm registry or a gem-source credential declares it there, and the declaration wins.
That is the whole design: machine-local state reaches a service only through a channel the Alphasfile names.

## What stays project-owned

Configuration committed to a service's repository is not ambient.
It is the same on every clone, so honoring it does not break reproducibility, and refusing it would break projects that rely on it.
zordon therefore leaves `.cargo/config.toml`, `.npmrc`, `.mvn/` and `.bundle/config` inside a checkout in force.
Ruby deserves a note: bundler ranks a local `.bundle/config` above the environment, so such a file can override zordon's `BUNDLE_PATH`; the build log reports it when one is present, since an untracked leftover in a `dir` primary is ambient state after all.

## Known limits

Yarn berry's `~/.yarnrc.yml` and bun's `~/.bunfig.toml` have no environment override upstream and remain HOME-bound.
Go's telemetry counters are written to the user config directory (`~/Library/Application Support/go/telemetry` on macOS, `~/.config/go/telemetry` elsewhere) by every `go` command; the location is not configurable through the environment, and the files carry no configuration, so they are left alone.
Bundler still reads `~/.gemrc` for proxy settings while `HOME` is in `sysenv`; only `gem install` skips it.
Git's own global configuration (`insteadOf` rewrites, credential helpers) applies to `git:` gems, cargo git dependencies and zordon's source checkouts alike; declaring `GIT_CONFIG_GLOBAL` or an SSH setup is the way to pin it, and zordon adds nothing ambient of its own.
