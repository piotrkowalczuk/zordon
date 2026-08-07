---
description: "Run several isolated copies of one stack side by side, each with its own state dir, its own picked ports and its own per-service git worktrees."
---

<div class="gh-canonical">Canonical version of this page: <a href="https://zordon.io/workspaces/">https://zordon.io/workspaces/</a></div>

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

**`create` vs `start`.** `zordon workspace create <name> [svc[@rev] …]` materializes the editable source: it `git worktree add`s each picked service (or *all* workspace-able services with no args) into `workspaces/<name>/src/<svc>` on a per-service branch `zordon/<name>/<svc>`.
For a `git` primary this bare-clones first; for a `src` primary it adds a worktree from your local repo (registered in *your* repo's `.git`, so your IDE / `git worktree list` sees it).
`zordon start` then runs the whole stack, reusing those checkouts and lazily materializing anything missing — a worktree for a picked service, a plain clone for an unpicked git-source one (see below).

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

Federation parents (below) are *reused* across workspaces — only the leaf forks.

### How a service's source is materialized

How a service's code lands in a workspace depends on whether it has a local source and whether you **picked** it (named it at `workspace create` / `workspace service add`).
Picking a service is what declares "I intend to edit this here":

| Service | Materialization | Branch |
|---|---|---|
| local `src {}`, **not** picked | built from the live source tree in place — your uncommitted edits just work | — |
| local `src {}` or `git {}`, **picked** | its own `git worktree` at `workspaces/<name>/src/<svc>`, so your IDE lists it and edits stay isolated | `zordon/<name>/<svc>` |
| `git {}`, **not** picked | third-party code: a plain `git clone` checked out at its ref — no branch, no worktree registration | — |
| no source (`install` / `pkg`) | a prebuilt binary / mise package — nothing is checked out | — |

The distinction that matters: an **unpicked** git-source service is third-party code you only run, so it is cloned, not worktree'd.
Because its clone carries no `zordon/<name>/<svc>` branch and registers no worktree, two workspaces (or an aborted / nested run) can never collide on a shared branch or admin dir.
To edit a git-source service, pick it — `zordon workspace create <name> <svc>` — and it graduates to an editable worktree.
`main` never picks anything, so in `main` every git-source service is a plain clone.

When a **picked** service's branch `zordon/<name>/<svc>` is already checked out elsewhere, you get a clear error (remove that workspace or point this one elsewhere) instead of a raw git failure.

zordon never overwrites code it finds in a checkout: a clean worktree on `zordon/<name>/<svc>` is reused silently, and its OWN init that a kill interrupted is rebuilt (there is no work in a checkout that never finished) — but a checkout you have taken onto another branch, or any tree it doesn't recognize as its own, is **reused as-is with a warning**, never reset or deleted. So switching branches in a workspace checkout, or leaving uncommitted work there, can't be lost to a `zordon start`. `zordon status` shows each service's checkout path and current branch, and flags one that isn't on its canonical `zordon/<name>/<svc>` branch.

### Where you can run zordon

Run it from anywhere in the tree — zordon resolves the invocation by walking **up**, like git from a subdir.
It climbs to the nearest workspace boundary (a `workspaces/<name>` dir or a `.workspace` marker) and then to the project `Alphasfile` at or above it, so a run from a plain project subdir attaches to `main` and a run from inside a workspace (including a service checkout under it) attaches to that workspace.
The `.workspace` marker is authoritative over its whole subtree: it **shadows** any `Alphasfile` a checked-out service repo carries, so such a buried file is never mistaken for a project root — which is what used to fork a nested `workspaces/…` stack whose branches collided with the real one.
The instance identity (`fs::hash()`, state dir) comes from the resolved root/workspace, not the raw cwd, so every subdir of a project maps to the same running stack.

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
