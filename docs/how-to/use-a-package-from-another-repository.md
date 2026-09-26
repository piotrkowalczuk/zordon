---
description: "Import a package from another git repository by its path and version, choose its features and inputs, and keep the version pinned in zordon.lock."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/use-a-package-from-another-repository/">https://zordon.io/how-to/use-a-package-from-another-repository/</a></div>

# Use a package from another repository

## 1. Import it by path and version

```hcl
import "github.com/piotrkowalczuk/zordon/examples/package/caddy@main" {
  features = ["hugo"]
}
```

The path is the repository followed by the package's directory inside it.
The version after `@` is a branch, a tag or a commit.

## 2. Pass inputs if the package takes any

```hcl
import "github.com/piotrkowalczuk/zordon/examples/package/hugo@main" {
  inputs = { title = "My site" }
}
```

`zordon plan` names any required input you left out and any input or feature the package does not declare.

## 3. Start

```sh
zordon start
```

zordon fetches the repository once, checks the commit out under `$ZORDON_HOME/mod`, and writes `zordon.lock` next to your Alphasfile.
Commit `zordon.lock`, so everyone runs the same commit.

## 4. Move to newer commits

```sh
zordon update
```

It prints each repository that moved and rewrites the lock.
Name repositories to update only some of them: `zordon update github.com/piotrkowalczuk/zordon`.

## Run a whole stack from a one-line Alphasfile

A stack that lives in a repository is a package too.
In an empty directory outside any project, a single line runs it:

```hcl
import "github.com/piotrkowalczuk/zordon/examples/package@main" {}
```

See [Packages](../alphasfile.md#packages) and [Remote imports](../alphasfile.md#remote-imports) for every rule.
