// Package source owns the "where does a service's code live" question.
//
// A service has a *primary* repository — either zordon-owned (`git`,
// bare-cloned under ~/.zordon/src) or user-owned (`dir`, an existing git
// checkout zordon never writes to). Every invocation gets its own working
// tree via `git worktree add` from that primary, so parallel invocations
// (workspaces) are isolated. Services with neither git nor dir (a crates.io
// crate, a prebuilt binary on $PATH) have no primary and are not
// workspaceable.
package source

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

type Kind string

const (
	KindNone Kind = ""    // crate / prebuilt — not workspaceable
	KindGit  Kind = "git" // zordon-owned bare primary
	KindDir  Kind = "dir" // user-owned repo primary
)

// Primary identifies a service's source of truth.
type Primary struct {
	Kind Kind
	// Repo is host/owner/repo (normalized) for KindGit, or an absolute
	// filesystem path for KindDir.
	Repo string
	// Ref is the default revision (branch/tag/rev) checked out into new
	// workspaces. Empty means the primary's current HEAD.
	Ref string
	// Workspace contains sparse checkout configuration.
	Workspace *Workspace
	// zordonHome is the host-wide state root resolved at startup. Used
	// to compute bare-clone paths (<zordonHome>/src/<repo>.git) without
	// reading the env in business logic.
	zordonHome string
}

type Workspace struct {
	Sparse []string
}

// NewPrimary builds a Primary from a service's git/dir/ref fields. git and
// dir are mutually exclusive; both empty ⇒ KindNone (not workspaceable).
// zordonHome is the resolved host-wide state root (caller's --home /
// ZORDON_HOME, defaulted to ~/.zordon at startup) used for bare-clone
// paths; pass "" only when no git primary is involved.
func NewPrimary(zordonHome, git, dir, ref string, workspace *Workspace) (Primary, error) {
	switch {
	case git != "" && dir != "":
		return Primary{}, errors.New("service declares both git and dir; pick one primary")
	case git != "":
		repo, err := normalizeGit(git)
		if err != nil {
			return Primary{}, err
		}
		return Primary{Kind: KindGit, Repo: repo, Ref: ref, Workspace: workspace, zordonHome: zordonHome}, nil
	case dir != "":
		abs, err := expandAbs(dir)
		if err != nil {
			return Primary{}, err
		}
		return Primary{Kind: KindDir, Repo: abs, Ref: ref, Workspace: workspace, zordonHome: zordonHome}, nil
	default:
		return Primary{Kind: KindNone, Ref: ref, Workspace: workspace, zordonHome: zordonHome}, nil
	}
}

// Workspaceable reports whether this service can get a git worktree.
func (p Primary) Workspaceable() bool { return p.Kind == KindGit || p.Kind == KindDir }

func normalizeGit(git string) (string, error) {
	g := strings.TrimPrefix(git, "https://")
	g = strings.TrimPrefix(g, "http://")
	g = strings.TrimSuffix(g, ".git")
	parts := strings.Split(g, "/")
	if len(parts) < 3 {
		return "", fmt.Errorf("git path too short: %q", git)
	}
	switch parts[0] {
	case "github.com", "gitlab.com", "bitbucket.org":
	default:
		return "", fmt.Errorf("unsupported git host %q (github.com, gitlab.com, bitbucket.org)", parts[0])
	}
	return strings.Join(parts[:3], "/"), nil
}

func expandAbs(dir string) (string, error) {
	if strings.HasPrefix(dir, "~") {
		home, err := zenv.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	return filepath.Abs(dir)
}

func (p Primary) cloneURL() string { return "https://" + p.Repo + ".git" }

// cloneSource is what `git clone` (Clone) reads from: the upstream URL for a
// git primary, the local repo path for a dir primary. Mirrors primaryPath's
// per-kind split.
func (p Primary) cloneSource() string {
	switch p.Kind {
	case KindGit:
		return p.cloneURL()
	case KindDir:
		return p.Repo
	}
	return ""
}

// BarePath is where the zordon-owned bare primary lives (KindGit only).
// The base dir is resolved at primary construction (see NewPrimary).
// Falls back to ".zordon" if zordonHome wasn't supplied — keeps the
// previous degraded-mode behavior for tests that construct a Primary
// without a real home.
func (p Primary) BarePath() string {
	base := p.zordonHome
	if base == "" {
		base = ".zordon"
	}
	return filepath.Join(base, "src", p.Repo) + ".git"
}

// primaryPath is the git dir we run `git worktree` against.
func (p Primary) primaryPath() string {
	switch p.Kind {
	case KindGit:
		return p.BarePath()
	case KindDir:
		return p.Repo
	}
	return ""
}

// Runner abstracts how a caller captures exec.Cmd output (e.g. piping into
// a per-service log stream). It must Start and Wait the command.
type Runner func(ctx context.Context, cmd *exec.Cmd) error

// Ensure makes the primary available. KindGit: bare-clone on first use,
// otherwise fetch all refs. KindDir: assert it's a git repo. KindNone: noop.
//
// The bare clone is a partial clone (--filter=blob:none): all refs and
// commit metadata are downloaded so any branch/tag/rev resolves, but blob
// content is fetched lazily on first checkout. This is fast on cold start
// AND `branch`/`tag`/`rev` pinning works without surprise — a plain
// --depth=1 clone only carries the default branch's HEAD and a tag like
// v2.14.0 would not exist in the bare repo.
func (p Primary) Ensure(ctx context.Context, run Runner) error {
	if run == nil {
		return errors.New("source.Ensure: nil runner")
	}
	switch p.Kind {
	case KindNone:
		return nil
	case KindDir:
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", p.Repo, "rev-parse", "--git-dir")); err != nil {
			return fmt.Errorf("dir primary %s is not a git repo: %w", p.Repo, err)
		}
		return nil
	case KindGit:
		bare := p.BarePath()
		// Legacy bare repos were created with --depth=1 (only the default
		// branch's HEAD), so a tag like v2.14.0 resolves to "invalid
		// reference". `fetch --unshallow` would deepen them but only by
		// pulling the entire history with blobs — minutes for a big repo —
		// because --filter only applies to repos that are already partial
		// clones. Faster and simpler: wipe and re-clone as a partial clone
		// (metadata-only, blobs lazy on first checkout).
		if zfs.Exists(bare) {
			if out, _ := exec.CommandContext(ctx, "git", "-C", bare,
				"rev-parse", "--is-shallow-repository").Output(); strings.TrimSpace(string(out)) == "true" {
				if err := zfs.RemoveTree(bare); err != nil {
					return fmt.Errorf("remove stale shallow bare %s: %w", bare, err)
				}
			}
		}
		if _, err := zfs.Stat(bare); zfs.IsMissingErr(err) {
			if err := zfs.EnsureDir(filepath.Dir(bare)); err != nil {
				return fmt.Errorf("mkdir bare parent: %w", err)
			}
			if err := run(ctx, exec.CommandContext(ctx, "git", "clone",
				"--bare", "--filter=blob:none", p.cloneURL(), bare)); err != nil {
				return fmt.Errorf("git clone --bare: %w", err)
			}
			return nil
		} else if err != nil {
			return err
		}
		// Refresh both heads and tags so newly-pushed refs become visible.
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", bare, "fetch",
			"--force", "origin",
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*")); err != nil {
			return fmt.Errorf("git fetch bare: %w", err)
		}
		return nil
	}
	return fmt.Errorf("unknown primary kind %q", p.Kind)
}

// branchWorktreePath returns the path of the worktree that currently has
// `branch` checked out (refs/heads/<branch>), if any. Read-only query; runs
// git directly (not via Runner) so the porcelain output can be parsed.
func branchWorktreePath(ctx context.Context, gitDir, branch string) (string, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", false
	}
	want := "branch refs/heads/" + branch
	var cur string
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = strings.TrimPrefix(line, "worktree ")
		case line == want && cur != "":
			return cur, true
		}
	}
	return "", false
}

// worktreeHealthy reports whether the worktree at dest is a COMPLETE, correctly
// registered checkout of branch — the only state safe to reuse as-is. Every
// condition must hold; any failure means the tree is unreadable, half-built, on
// the wrong branch, or (under prune/re-add churn across workspaces)
// cross-linked to a DIFFERENT workspace's admin dir and silently on its branch
// (the issue #73 comment). Such a tree is rebuilt, never trusted.
//
//  1. dest/.git resolves to an admin dir — git can read the worktree at all.
//     (This is what the old bare "does .git exist" check missed: a .git gitlink
//     dangling into a reassigned admin dir still "exists" but is unreadable.)
//  2. that admin dir is a worktree admin dir (its parent is `worktrees/`), not
//     the bare/repo git-dir itself.
//  3. the admin dir's `gitdir` back-pointer names dest/.git — the gitlink and
//     the registration agree. This is the invariant a cross-link violates: the
//     orphaned checkout's .git points at an admin dir whose gitdir names a
//     DIFFERENT working tree.
//  4. HEAD is `ref: refs/heads/<branch>` — on the branch we expect (AddWorktree
//     always creates with -B, so a healthy reuse target is never detached).
//  5. no `locked` marker — AddWorktree clears it only on a finished checkout,
//     so a lingering lock means a killed init never completed.
//
// The `locked`/back-pointer/HEAD facts are read as files, NOT parsed from
// `git worktree list --porcelain`, whose `locked`/HEAD columns only landed in
// git 2.36 — the older distro gits in CI would silently pass.
func worktreeHealthy(ctx context.Context, dest, branch string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", dest, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return false
	}
	adminDir := strings.TrimSpace(string(out))
	if filepath.Base(filepath.Dir(adminDir)) != "worktrees" {
		return false
	}
	if zfs.Exists(filepath.Join(adminDir, "locked")) {
		return false
	}
	back, err := zfs.Read(filepath.Join(adminDir, "gitdir"))
	if err != nil || !samePath(strings.TrimSpace(string(back)), filepath.Join(dest, ".git")) {
		return false
	}
	head, err := zfs.Read(filepath.Join(adminDir, "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+branch {
		return false
	}
	return true
}

// samePath reports whether two paths name the same location, tolerating
// symlinked ancestors (macOS /var → /private/var) and un-cleaned forms.
func samePath(a, b string) bool {
	return normPath(a) == normPath(b)
}

// normPath canonicalizes p by resolving symlinks on its deepest EXISTING
// ancestor and re-attaching the missing tail. A plain EvalSymlinks returns
// empty for a path whose leaf is gone — e.g. a worktree dir already `rm`-ed
// (#40) — which would then fail to normalize /var → /private/var and read as a
// spurious mismatch against git's /private/var registration.
func normPath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// AddWorktree creates (or reuses) a working tree at dest, on branch
// `branch`, starting from p.Ref (or the primary's HEAD). A finished worktree
// is reused as-is so parallel invocations stay stable across restarts; one
// left half-built by a killed init is reclaimed and rebuilt.
func (p Primary) AddWorktree(ctx context.Context, dest, branch string, run Runner) error {
	if !p.Workspaceable() {
		return fmt.Errorf("service has no git/dir primary; not workspaceable")
	}
	if run == nil {
		return errors.New("source.AddWorktree: nil runner")
	}
	gitDir := p.primaryPath()
	// A complete, correctly-registered worktree on this branch is reused as-is
	// (worktreeHealthy validates the full gitlink↔admin↔branch chain, not just
	// that .git exists). Anything else — half-built, cross-linked, on the wrong
	// branch — falls through to reclaim + rebuild.
	if worktreeHealthy(ctx, dest, branch) {
		return nil
	}
	if err := zfs.EnsureDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	start := p.Ref
	if start == "" {
		start = "HEAD"
	}

	// Reclaim a leftover registration of <branch> before re-adding. A setup
	// killed mid-init leaves the worktree LOCKED with the `initializing`
	// sentinel; the working dir may still be present (incomplete checkout,
	// #38) or already cleaned away while the bare's registration survives
	// (#40). A locked entry blocks both `worktree prune` (it skips locked
	// entries) and `worktree add -B` (the branch stays "held" at the dead
	// path), so unlock, then `remove --force` (reclaims a dir-present entry)
	// and `prune` (reclaims a dir-gone one). Gated on branchWorktreePath so a
	// fresh add stays quiet (no "fatal: not a working tree" from remove). If
	// <branch> is STILL registered afterwards, it's a live worktree
	// elsewhere — a real collision, surfaced clearly. Matching is by branch,
	// never by path, to stay immune to /var↔/private/var symlink differences.
	if regPath, registered := branchWorktreePath(ctx, gitDir, branch); registered {
		// Only unlock+remove when <branch> is registered at THIS dest — a
		// killed init (#38) or an out-of-band `rm -rf` of the tree (#40). When
		// it points elsewhere, `remove --force dest` would log a misleading
		// "fatal: '<dest>' is not a working tree" (issue #73 logs); prune alone
		// clears a stale dir-gone entry, and a surviving one is a real
		// collision surfaced below.
		if samePath(regPath, dest) {
			_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "unlock", dest))
			_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "remove", "--force", dest))
		}
		_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "prune"))
		if other, still := branchWorktreePath(ctx, gitDir, branch); still {
			return fmt.Errorf("branch %q is already checked out at %s; "+
				"remove that worktree first (e.g. `zordon workspace rm`) or point this one elsewhere",
				branch, other)
		}
	}

	// Create the worktree locked and unchecked-out: `--lock` marks it
	// atomically at creation, so a failfast kill never leaves an
	// unlocked-but-incomplete tree, and `--no-checkout` defers the
	// interruptible checkout to run while locked. `--reason` is deliberately
	// omitted — `git worktree add` only learned it in 2.34 and the distro
	// gits in CI reject it. The lock is the completion sentinel, cleared only
	// on success below.
	args := []string{"-C", gitDir, "worktree", "add", "--lock", "--no-checkout", "--force", "-B", branch, dest, start}
	if err := run(ctx, exec.CommandContext(ctx, "git", args...)); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}

	if p.Workspace != nil && len(p.Workspace.Sparse) > 0 {
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", dest, "sparse-checkout", "init")); err != nil {
			return fmt.Errorf("git sparse-checkout init: %w", err)
		}
		sparseArgs := append([]string{"-C", dest, "sparse-checkout", "set"}, p.Workspace.Sparse...)
		if err := run(ctx, exec.CommandContext(ctx, "git", sparseArgs...)); err != nil {
			return fmt.Errorf("git sparse-checkout set: %w", err)
		}
	}

	if err := run(ctx, exec.CommandContext(ctx, "git", "-C", dest, "checkout", branch)); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}

	// Checkout finished; clear the lock so the next start reuses the tree
	// instead of rebuilding it. This unlock is the commit point — until it
	// runs, the worktree reads as not-yet-finished.
	if err := run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "unlock", dest)); err != nil {
		return fmt.Errorf("git worktree unlock: %w", err)
	}
	return nil
}

// cloneRefFile is the completion sentinel Clone drops inside a finished
// clone's .git dir, recording the ref the tree was checked out at. It lives
// under .git so it never shows up as an untracked file in the working tree,
// and RemoveTree(dest) wipes it along with the rest on a rebuild.
const cloneRefFile = "zordon-clone-ref"

// Clone materializes a third-party (unpicked git-source) service at dest by
// cloning its primary directly and checking it out DETACHED at ref. Unlike
// AddWorktree it registers no worktree and creates no branch, so nested or
// parallel checkouts of the same service across workspaces can never collide on
// a shared admin dir or a `zordon/<ws>/<svc>` branch (issue #73). This is the
// non-editable path: a service becomes editable (and gets a real worktree) only
// by being picked at `workspace create`.
//
// In production only KindGit reaches here (a KindDir service that is not picked
// runs in place; a picked service of either kind goes through AddWorktree), and
// it clones the upstream URL. The method is defined over any workspaceable
// primary — cloning a KindDir repo by path is the same operation — so the whole
// clone/reuse/rebuild path is exercisable without network.
func (p Primary) Clone(ctx context.Context, dest, ref string, run Runner) error {
	if !p.Workspaceable() {
		return fmt.Errorf("source.Clone: service has no git/dir primary; nothing to clone")
	}
	if run == nil {
		return errors.New("source.Clone: nil runner")
	}
	want := ref
	if want == "" {
		want = "HEAD"
	}
	// A finished clone at the same ref is reused as-is (the sentinel is
	// written last, on success). A different ref, a half-done clone, or a
	// worktree left by an older zordon all fall through to a clean rebuild:
	// `git clone` + `checkout --detach <ref>` handles branch/tag/rev
	// uniformly, so re-materializing is simpler and more robust than trying
	// to fetch-and-move an existing tree onto an arbitrary ref.
	if cur, ok := readCloneRef(dest); ok && cur == want {
		return nil
	}
	if err := p.clearStaleCheckout(ctx, dest, run); err != nil {
		return err
	}
	if err := zfs.EnsureDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("mkdir checkout parent: %w", err)
	}
	// --filter=blob:none keeps origin as a promisor remote, so the detached
	// checkout lazily fetches blobs from the source. (A `git clone --local` off
	// zordon's own partial bare would NOT carry the promisor and would fail to
	// read any blob — hence cloning the upstream directly.)
	if err := run(ctx, exec.CommandContext(ctx, "git", "clone",
		"--filter=blob:none", "--no-checkout", p.cloneSource(), dest)); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if p.Workspace != nil && len(p.Workspace.Sparse) > 0 {
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", dest, "sparse-checkout", "init")); err != nil {
			return fmt.Errorf("git sparse-checkout init: %w", err)
		}
		sparseArgs := append([]string{"-C", dest, "sparse-checkout", "set"}, p.Workspace.Sparse...)
		if err := run(ctx, exec.CommandContext(ctx, "git", sparseArgs...)); err != nil {
			return fmt.Errorf("git sparse-checkout set: %w", err)
		}
	}
	if err := run(ctx, exec.CommandContext(ctx, "git", "-C", dest, "checkout", "--detach", want)); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	// Commit point: record the ref so the next start reuses the tree instead
	// of re-cloning.
	if err := zfs.AtomicWrite(filepath.Join(dest, ".git", cloneRefFile), []byte(want)); err != nil {
		return fmt.Errorf("write clone sentinel: %w", err)
	}
	return nil
}

// clearStaleCheckout removes whatever currently sits at dest so Clone can
// rebuild. A checkout left by an older zordon is a registered worktree (its
// .git is a gitlink into the bare), so before deleting the tree we de-register
// it from the bare — otherwise the bare keeps the branch "held" and a later
// `workspace create` of the same service would hit "already checked out". Both
// git calls are best-effort: they no-op cleanly when dest is a plain clone or
// absent.
func (p Primary) clearStaleCheckout(ctx context.Context, dest string, run Runner) error {
	if gd := p.primaryPath(); zfs.Exists(gd) {
		_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gd, "worktree", "remove", "--force", dest))
		_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gd, "worktree", "prune"))
	}
	if !zfs.Exists(dest) {
		return nil
	}
	if err := zfs.RemoveTree(dest); err != nil {
		return fmt.Errorf("clear stale checkout %s: %w", dest, err)
	}
	return nil
}

// readCloneRef returns the ref recorded by a finished Clone, if present.
func readCloneRef(dest string) (string, bool) {
	b, err := zfs.Read(filepath.Join(dest, ".git", cloneRefFile))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// RemoveWorktree detaches a worktree from its primary and deletes the tree.
func (p Primary) RemoveWorktree(ctx context.Context, dest string, run Runner) error {
	if run == nil {
		return errors.New("source.RemoveWorktree: nil runner")
	}
	if gd := p.primaryPath(); gd != "" {
		_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gd, "worktree", "remove", "--force", dest))
	}
	return zfs.RemoveTree(dest)
}
