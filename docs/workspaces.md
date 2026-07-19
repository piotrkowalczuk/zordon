# Workspaces

Every run of zordon happens in a *workspace*. The project root is the
implicit workspace `main`; you can spin up more, each a fully isolated
copy of the whole stack over the **same `Alphasfile`** — its own state
dir, its own `pickport()` draws, its own per-service `git worktree`
checkouts, its own `alpha`.

```sh
zordon workspace create feature        # checks out every workspace-able
                                      # service into workspaces/feature
zordon workspace create feature api    # only `api` (others run from upstream)
zordon workspace create feature api@v2 # …pin `api` to revision v2
cd workspaces/feature
zordon start                          # walks up, adopts ../../../Alphasfile
                                      # as the leaf — but as workspace "feature"
zordon workspace list
zordon workspace service add --workspace=feature --services=api  # add a checkout
zordon workspace service rm  --workspace=feature --services=api  # drop one
zordon workspace rm feature
```

**`create` vs `start`.** `zordon workspace create <name> [svc[@rev] …]`
materializes the editable source: it `git worktree add`s each picked
service (or *all* workspace-able services with no args) into
`workspaces/<name>/src/<svc>` on a branch `zordon/<name>`. For a
`git` primary this bare-clones first; for a `src` primary it adds a
workspace from your local repo (registered in *your* repo's `.git`, so
your IDE / `git worktree list` sees it). `zordon start` then runs the
whole stack, reusing those checkouts (and lazily creating any missing
ones).

**Per-service add/rm.** To adjust an existing workspace without recreating it, `zordon workspace service add --workspace=<name> --services=<svc[@rev],…>` materializes more service checkouts (on the same branch and path `start` expects), and `zordon workspace service rm --workspace=<name> --services=<svc,…>` detaches them (the git worktree is removed and its tree deleted).
Both reject `--workspace=main`, which has no per-service checkout.

`zordon start` from `workspaces/<name>/` walks up, finds the
project's `Alphasfile`, and adopts it as the leaf — same file on disk,
different invocation. Every level carries two short hashes:

- **`fs::hash()`** — `sha(invocation_dir)`. Identifies *which alpha
  instance* this is; names the socket / tmp dir. `main` and `feature`
  get distinct `fs::hash()` ⇒ distinct sockets, state dirs, and fresh
  `pickport()` values — they run side by side without colliding.
- **`cfg::hash()`** — `sha(alphasfile_bytes + parent_context)`. Identifies
  *the manifest content*. Changes when you edit the Alphasfile (or a
  parent's resolved services change). Drives federation drift detection.

`zordon status` shows each level's `fs::hash()`.

Each workspace-able service is materialized via `git worktree add` from
its primary and built there, so editing code in one workspace's checkout
doesn't touch another's. If the branch `zordon/<name>` is already
checked out at a different path, you get a clear error (remove that
workspace or point this one elsewhere) instead of a raw git failure.
Federation parents (below) are *reused* across workspaces — only the
leaf forks.

### The `.workspace` marker

`zordon workspace create` drops an empty `.workspace` file into
`workspaces/<name>/` — an explicit, durable "this directory is a workspace"
signal for zordon and for agents/tooling that would otherwise have to guess
from the path.
The `<root>/workspaces/<name>/` path stays authoritative, so editing or
deleting the marker never un-workspaces a conventional workspace; the marker
is an additive signal, not a single point of failure.
`main` is the project root and carries no marker.

### Partial checkout (`sparse`)

For a big monorepo you rarely want the whole tree. A `workspace { sparse
= [...] }` block makes the checkout a git **sparse cone**, materializing
only the listed directories (paths relative to the primary repo root):

```hcl
service "go" "example" {
  src {
    path = "../.."                            # primary = the repo this file is in
    exe  = "./examples/workspace/src/example"  # build target, repo-root-relative
  }
  vars = { port = net::pickport() }
  workspace {
    sparse = ["examples/workspace/src/example"]
  }
  runtime {
    cmd = ["${fs::bin()}/example", "-addr", "127.0.0.1:${self.vars.port}"]
  }
}
```

The checkout then holds only that subtree — plus the repo's **top-level
files** (`go.mod`, `go.sum`, …). That's inherent to git cone mode and
desirable: `go.mod` at the module root is needed to build anyway. See
[examples/workspace](https://github.com/piotrkowalczuk/zordon/tree/main/examples/workspace).

Main use case: an AI agent gets a sandbox next to the developer's stack;
derivatives: parallel experiments, A/B-testing two revisions. For async
isolation, keep the broker (NATS/Kafka/…) in the **project** Alphasfile,
not the global parent — then each workspace gets its own bus.
