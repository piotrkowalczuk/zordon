---
title: "Ruby services"
description: "Ruby services install their gems into a per-checkout vendor path with bundler, then run through an explicit command — there is no binary to infer."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/services/ruby/">https://zordon.io/services/ruby/</a></div>

# Ruby services

```hcl
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

`git` or `src`, like Go (no `crate`). `branch`/`tag`/`rev` pin the
revision; relative `src` resolves against the Alphasfile's directory.

## Build & run

The default "build" is dependency install in the checkout:

```sh
bundle config set --local path vendor/bundle && bundle install
```

(`--path` was removed in Bundler 2.x, so the path is written to the
per-checkout `.bundle/config` first — which also lets `bundle exec` find
the gems at runtime.)

Ruby has no single binary, so the run command is **not** inferred —
give an explicit `runtime { cmd = [...] }` (e.g. `bundle exec ...`,
`rails server`, `rackup`). It runs with cwd = the per-invocation
checkout, so `Gemfile`/app files resolve. Override the install step
with `build { cmd = [...] }` if `bundle install` isn't what you want.

## Logs

Ruby stack traces are noisy; a `log { filter = "<expression>" }` block
drops matching lines (e.g. the `from …` backtrace frames) at the source,
before they reach the streamed output or alpha's log.
The filter is a small predicate DSL — `hasPrefix`/`contains`/`matches` on
the raw line, plus `json`/`logfmt`/`severity` for structured fields — see
the [log filter reference](../reference/log-filter.md).
