---
description: "Give a Ruby service credentials for a private gem source through a declared dotenv channel instead of ~/.bundle/config, so a fresh clone plus one secret file reproduces the build."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/authenticate-to-a-private-gem-source/">https://zordon.io/how-to/authenticate-to-a-private-gem-source/</a></div>

# Authenticate to a private gem source

A Ruby service under zordon never reads `~/.bundle/config` or `~/.gem/credentials`, so a credential stored there does not reach `bundle install`.
Declare it instead.

## 1. Name the source in the Gemfile

```ruby
source "https://gems.example.com" do
  gem "internal-gem"
end
```

## 2. Put the credential in a gitignored dotenv file

Bundler reads a credential for a host from `BUNDLE_<HOST>`, with dots written as `__` and dashes as `___`.
For `gems.example.com` that is `BUNDLE_GEMS__EXAMPLE__COM`.

```sh
# .env — gitignored, one per developer
BUNDLE_GEMS__EXAMPLE__COM=deploy:s3cr3t
```

Add `.env` to the repository's `.gitignore` and document the one line each developer has to create.

## 3. Declare the dotenv on the service

```hcl
service "ruby" "api" {
  git { url = "github.com/acme/api" }

  runtime {
    dotenv = ["${fs::src()}/.env"]
    cmd    = ["bundle", "exec", "rails", "server", "-p", "${self.vars.port}"]
  }
}
```

The dotenv is applied to the runtime and to provisions.
The build phase is hermetic and skips dotenv files, so a credential the install itself needs goes on the build instead:

```hcl
  build {
    env = { BUNDLE_GEMS__EXAMPLE__COM = os::env("BUNDLE_GEMS__EXAMPLE__COM") }
  }
```

`os::env` reads the value from the shell that runs `zordon start`, so this form needs the variable exported there rather than a file.
Either way, the credential never depends on a file under `HOME`, and a missing one fails the install loudly instead of silently building against the public index.

## `git:` gems

A gem sourced from a git URL is fetched by git, not by bundler, so bundler's credential keys do not apply.
Use an SSH URL and rely on the developer's SSH agent, or set `GIT_CONFIG_GLOBAL` to a committed config with the `insteadOf` rewrite the project expects:

```hcl
  build {
    env = { GIT_CONFIG_GLOBAL = "${fs::src()}/.gitconfig.zordon" }
  }
```

zordon does not add or strip any git configuration of its own, so what git sees is what the Alphasfile declares plus the developer's global config.
