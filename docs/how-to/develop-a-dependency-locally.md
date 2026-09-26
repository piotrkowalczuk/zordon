---
description: "Point zordon at a local checkout of a repository your stack imports remotely, without changing the Alphasfile or the lock."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/how-to/develop-a-dependency-locally/">https://zordon.io/how-to/develop-a-dependency-locally/</a></div>

# Develop a dependency locally

## 1. Make sure the checkout declares its identity

The repository needs a `zordon.mod` at its root:

```hcl
module = "github.com/acme/infra"
```

Without it, put the checkout in a `<host>/<owner>/<repo>` layout instead, such as `~/src/github.com/acme/infra`, and search `~/src`.

## 2. Add a zordon.work above where you run zordon

```hcl
search "~/code/infra" {}
```

Put it in your working directory or any directory above it, and do not commit it.
Every import of `github.com/acme/infra/...` now reads your checkout's current files, whatever version the import names.

## 3. Check where each import comes from

```sh
zordon plan
```

The header shows `(search ~/code/infra)` next to every import your checkout provides.
Edits in the checkout restart the stack on the next `zordon start`, like edits to the Alphasfile.

## 4. Go back to the pinned version

Remove the `search` entry, or the whole `zordon.work`.
`zordon.lock` was never touched, so the next start uses the pinned commit again.

See [zordon.work](../reference/files.md#zordonwork) for every rule.
