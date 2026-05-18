# Worktrees

Every run of zordon happens in a *worktree*. The project root is the
implicit worktree `main`; you can spin up more, each a fully isolated
copy of the whole stack over the **same `Alphasfile`** — its own state
dir, its own `pickport()` draws, its own per-service `git worktree`
checkouts, its own `alpha`.

```sh
zordon worktree create feature        # checks out every worktree-able
                                      # service into .zordon/worktrees/feature
zordon worktree create feature api    # only `api` (others run from upstream)
zordon worktree create feature api@v2 # …pin `api` to revision v2
cd .zordon/worktrees/feature
zordon start                          # walks up, adopts ../../../Alphasfile
                                      # as the leaf — but as worktree "feature"
zordon worktree list
zordon worktree rm feature
```

**`create` vs `start`.** `zordon worktree create <name> [svc[@rev] …]`
materializes the editable source: it `git worktree add`s each picked
service (or *all* worktree-able services with no args) into
`.zordon/worktrees/<name>/src/<svc>` on a branch `zordon/<name>`. For a
`git` primary this bare-clones first; for a `src` primary it adds a
worktree from your local repo (registered in *your* repo's `.git`, so
your IDE / `git worktree list` sees it). `zordon start` then runs the
whole stack, reusing those checkouts (and lazily creating any missing
ones).

`zordon start` from `.zordon/worktrees/<name>/` walks up, finds the
project's `Alphasfile`, and adopts it as the leaf — same file on disk,
different invocation. The invocation hash is
`sha(invocation_dir + alphasfile_bytes + parent_context)`, so `main` and
`feature` get **distinct hashes ⇒ distinct sockets, state dirs, and
fresh `pickport()` values** — they run side by side without colliding.
`zordon status` shows which worktree each level is and its hash; that
hash is also what `pathhash()` returns.

Each worktree-able service is materialized via `git worktree add` from
its primary and built there, so editing code in one worktree's checkout
doesn't touch another's. If the branch `zordon/<name>` is already
checked out at a different path, you get a clear error (remove that
worktree or point this one elsewhere) instead of a raw git failure.
Federation parents (below) are *reused* across worktrees — only the
leaf forks.

### Partial checkout (`sparse`)

For a big monorepo you rarely want the whole tree. A `worktree { sparse
= [...] }` block makes the checkout a git **sparse cone**, materializing
only the listed directories (paths relative to the primary repo root):

```hcl
service "go" "example" {
  src = "../.."                              # primary = the repo this file is in
  exe = "./examples/worktree/src/example"    # build target, repo-root-relative
  worktree {
    sparse = ["examples/worktree/src/example"]
  }
  cmd = ["${fs::bin()}/example", "-addr", "127.0.0.1:${self.vars.port}"]
}
```

The checkout then holds only that subtree — plus the repo's **top-level
files** (`go.mod`, `go.sum`, …). That's inherent to git cone mode and
desirable: `go.mod` at the module root is needed to build anyway. See
[examples/worktree](https://github.com/piotrkowalczuk/zordon/tree/master/examples/worktree).

Main use case: an AI agent gets a sandbox next to the developer's stack;
derivatives: parallel experiments, A/B-testing two revisions. For async
isolation, keep the broker (NATS/Kafka/…) in the **project** Alphasfile,
not the global parent — then each worktree gets its own bus.
