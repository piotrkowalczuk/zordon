---
description: "Require a repository at a version, import a package from it by path, choose its features and inputs, and keep the version pinned in zordon.lock."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/use-a-package-from-another-repository/">https://zordon.io/how-to/use-a-package-from-another-repository/</a></div>

# Use a package from another repository

## 1. Require the repository

```sh
zordon pkg get github.com/piotrkowalczuk/zordon@main
```

It writes `require "github.com/piotrkowalczuk/zordon" { ref = "main" }` into the `zordon.mod` above your Alphasfile, or into the Alphasfile itself when there is none.
The ref is a branch, a tag or a commit.

## 2. Import the package by path

```hcl
import "github.com/piotrkowalczuk/zordon/examples/package/caddy" {
  features = ["dns"]
}
```

The path is the repository followed by the package's directory inside it, with no version.

## 3. Pass inputs if the package takes any

```hcl
import "github.com/piotrkowalczuk/zordon/examples/package/shop" {
  inputs = { title = "My shop" }
}
```

`zordon plan` names any required input you left out and any input or feature the package does not declare.
The package's own `inputs` and `features` lines list what it accepts, and each feature says what it turns on.

## 4. Start

```sh
zordon start
```

zordon fetches the repository once, checks the commit out under `$ZORDON_HOME/mod`, and writes `zordon.lock` next to your Alphasfile.
Commit `zordon.lock`, so everyone runs the same commit.

## 5. Move to newer commits

```sh
zordon pkg update
```

It prints each repository that moved and rewrites the lock.
Name repositories to update only some of them: `zordon pkg update github.com/piotrkowalczuk/zordon`.
To switch to another branch or tag, run `zordon pkg get` again with the new ref.

## Run a whole stack from a two-line Alphasfile

A stack that lives in a repository is a package too: a `package` block that imports other packages.
In an empty directory outside any project, two lines run it:

```hcl
require "github.com/piotrkowalczuk/zordon" { ref = "main" }
import "github.com/piotrkowalczuk/zordon/examples/package" {}
```

See [examples/remote](https://github.com/piotrkowalczuk/zordon/tree/main/examples/remote) for `zordon pkg get`, the lock and `zordon pkg update` in one runnable script.
See [Packages](../alphasfile.md#packages) and [Remote imports](../alphasfile.md#remote-imports) for every rule.
