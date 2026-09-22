---
title: "Define a Ruby service that runs via bundler"
description: "Ruby services install their gems out of tree with bundler under a hermetic, mise-pinned gem environment, then run through an explicit command — there is no binary to infer."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/ruby/">https://zordon.io/services/ruby/</a></div>

# Define a Ruby service that runs via bundler

```hcl
toolchain {
  ruby {
    version = "3.3.6"
    tools   = { bundler = "2.5.6" }
  }
}

service "ruby" "ruby-service" {
  git {
    url = "github.com/niwasawa/ruby-sinatra-hello-world"
  }

  vars = { port = net::pickport() }
  log {
    format = "plain"
    filter = <<-EOT
      hasPrefix(line, "\tfrom ") or hasPrefix(line, "/Users/")
    EOT
  }

  runtime {
    cmd = ["bundle", "exec", "ruby", "myapp.rb", "-p", "${self.vars.port}"]
  }
}
```

## Source

`git` or `src`, like Go (no `crate`).
`branch`/`tag`/`rev` pin the revision; relative `src` resolves against the Alphasfile's directory.

## Build & run

The default "build" is dependency install:

```sh
bundle install
```

It runs with `BUNDLE_PATH` set to `<state>/bundle/<service>`, so the gems land outside the checkout and nothing is written into the source tree.
The same `BUNDLE_PATH` reaches the runtime command and every provision, which is what lets `bundle exec` find the gems.

Ruby has no single binary, so the run command is **not** inferred — give an explicit `runtime { cmd = [...] }` (e.g. `bundle exec ...`, `rails server`, `rackup`).
It runs with cwd = the per-invocation checkout, so `Gemfile`/app files resolve.
Override the install step with `build { cmd = [...] }` if `bundle install` isn't what you want.

## Gem environment

The pinned ruby's gem world is the only one a service sees, and bundler's per-user state is zordon-owned.
`GEM_HOME` and `GEM_PATH` point at the mise install's own gem dir, so a `gem install --user-install` on the host (in `~/.gem/ruby/<abi>`) is invisible.
`BUNDLE_USER_HOME` moves bundler's global config, compact-index cache and plugins out of `~/.bundle`.
A `bundler` entry under `toolchain.ruby.tools` installs that version into the pinned ruby and sets `BUNDLER_VERSION`, so the `bundle` shim runs exactly it; without one, the lockfile's `BUNDLED WITH` governs and bundler installs that version into `GEM_HOME` on first use (network).
`gem install` for declared tools runs with `--norc`, so a `~/.gemrc` cannot redirect it.

A checkout's own `.bundle/config` is project state and stays in force.
Bundler ranks it above the environment, so it can override `BUNDLE_PATH` and the other pins; the build log reports it when present.
Every one of these values sits in the toolchain tier, so `toolchain { ruby { env } }` and a service's `env` still override them.

Credentials for a private gem source belong in a declared channel, never in `~/.bundle/config` or `~/.gem/credentials`: see [Authenticate to a private gem source](../how-to/authenticate-to-a-private-gem-source.md).
Why the environment is closed this way is covered in [Hermetic toolchains](../explanation/hermetic-toolchains.md).

## Logs

Ruby stack traces are noisy; a `log { filter = "<expression>" }` block drops matching lines (e.g. the `from …` backtrace frames) at the source, before they reach the streamed output or alpha's log.
The filter is a small predicate DSL — `hasPrefix`/`contains`/`matches` on the raw line, plus `json`/`logfmt`/`severity` for structured fields — see the [log filter reference](../reference/log-filter.md).
