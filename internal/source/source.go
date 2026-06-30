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

// AddWorktree creates (or reuses) a working tree at dest, on branch
// `branch`, starting from p.Ref (or the primary's HEAD). Reuses an existing
// valid worktree as-is so parallel invocations stay stable across restarts.
func absOr(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
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

// worktreeLocked reports whether the worktree at dest is locked. AddWorktree
// creates worktrees locked and clears the lock only once the checkout
// finishes, so a still-locked worktree means a prior setup (e.g. one killed
// by failfast) never completed — the tree is incomplete and must be rebuilt,
// not reused.
//
// Detection reads the `locked` marker file in the worktree's admin dir,
// which git has written since worktree locking landed (2.7). It deliberately
// does NOT parse `git worktree list --porcelain`: that output only grew a
// `locked` line in git 2.36, so on the older distro gits in CI it would
// always report "not locked" and the heal would silently no-op.
func worktreeLocked(ctx context.Context, dest string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", dest, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return false
	}
	adminDir := strings.TrimSpace(string(out))
	return zfs.Exists(filepath.Join(adminDir, "locked"))
}

func (p Primary) AddWorktree(ctx context.Context, dest, branch string, run Runner) error {
	if !p.Workspaceable() {
		return fmt.Errorf("service has no git/dir primary; not workspaceable")
	}
	if run == nil {
		return errors.New("source.AddWorktree: nil runner")
	}
	gitDir := p.primaryPath()
	if zfs.Exists(filepath.Join(dest, ".git")) {
		if !worktreeLocked(ctx, dest) {
			return nil // checkout finished cleanly — reuse
		}
		// A prior setup died mid-init (e.g. failfast killed it before the
		// checkout finished); the lock is git's durable "not done" marker.
		// Unlock so remove can run, drop the half-baked tree, and fall
		// through to rebuild it.
		_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "unlock", dest))
		_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "remove", "--force", dest))
	}
	if err := zfs.EnsureDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	start := p.Ref
	if start == "" {
		start = "HEAD"
	}

	// If the directory was manually deleted but Git still tracks it, a later
	// `worktree add` fails. `worktree prune` silently drops stale admin
	// entries and is a no-op otherwise (unlike `worktree remove <dest>`,
	// which prints a scary "fatal: not a working tree" when dest isn't one).
	_ = run(ctx, exec.CommandContext(ctx, "git", "-C", gitDir, "worktree", "prune"))

	// prune only reclaims worktrees whose directory vanished. If the branch
	// is still checked out at a *different* live path, `worktree add -B`
	// would hard-fail with a raw git fatal — surface a clear error instead.
	if other, ok := branchWorktreePath(ctx, gitDir, branch); ok {
		if a, _ := filepath.Abs(other); filepath.Clean(a) != filepath.Clean(absOr(dest)) {
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
