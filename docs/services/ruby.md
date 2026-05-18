# Ruby services

```hcl
service "ruby" "ruby-service" {
  git = "github.com/niwasawa/ruby-sinatra-hello-world"

  vars = { port = net::pickport() }
  log  { format = "plain"  filter = "^\\tfrom .*" }

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
bundle install --path vendor/bundle
```

Ruby has no single binary, so the run command is **not** inferred —
give an explicit `runtime { cmd = [...] }` (e.g. `bundle exec ...`,
`rails server`, `rackup`). It runs with cwd = the per-invocation
checkout, so `Gemfile`/app files resolve. Override the install step
with `build { cmd = [...] }` if `bundle install` isn't what you want.

## Logs

Ruby stack traces are noisy; `log { format = "plain" filter = "<regex>" }`
drops matching lines (e.g. the `from …` backtrace frames) from the
streamed output.
