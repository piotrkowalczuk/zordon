// Package source owns the "where does a service's code live" question.
//
// A service has a *primary* repository — either zordon-owned (`git`,
// bare-cloned under ~/.zordon/src) or user-owned (`dir`, an existing git
// checkout zordon never writes to). Every invocation gets its own working
// tree via `git worktree add` from that primary, so parallel invocations
// (worktrees) are isolated. Services with neither git nor dir (a crates.io
// crate, a prebuilt binary on $PATH) have no primary and are not
// worktree-able.
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Kind string

const (
	KindNone Kind = ""    // crate / prebuilt — not worktree-able
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
	// worktrees. Empty means the primary's current HEAD.
	Ref string
	// Worktree contains sparse checkout configuration.
	Worktree *Worktree
}

type Worktree struct {
	Sparse []string
}

// Home returns the on-disk root for zordon-managed source.
func Home() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".zordon")
	}
	return ".zordon"
}

// NewPrimary builds a Primary from a service's git/dir/ref fields. git and
// dir are mutually exclusive; both empty ⇒ KindNone (not worktree-able).
func NewPrimary(git, dir, ref string, worktree *Worktree) (Primary, error) {
	switch {
	case git != "" && dir != "":
		return Primary{}, errors.New("service declares both git and dir; pick one primary")
	case git != "":
		repo, err := normalizeGit(git)
		if err != nil {
			return Primary{}, err
		}
		return Primary{Kind: KindGit, Repo: repo, Ref: ref, Worktree: worktree}, nil
	case dir != "":
		abs, err := expandAbs(dir)
		if err != nil {
			return Primary{}, err
		}
		return Primary{Kind: KindDir, Repo: abs, Ref: ref, Worktree: worktree}, nil
	default:
		return Primary{Kind: KindNone, Ref: ref, Worktree: worktree}, nil
	}
}

// Worktreeable reports whether this service can get a git worktree.
func (p Primary) Worktreeable() bool { return p.Kind == KindGit || p.Kind == KindDir }

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
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	return filepath.Abs(dir)
}

func (p Primary) cloneURL() string { return "https://" + p.Repo + ".git" }

// BarePath is where the zordon-owned bare primary lives (KindGit only).
func (p Primary) BarePath() string {
	return filepath.Join(Home(), "src", p.Repo) + ".git"
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
		if _, err := os.Stat(bare); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
				return fmt.Errorf("mkdir bare parent: %w", err)
			}
			if err := run(ctx, exec.CommandContext(ctx, "git", "clone", "--bare", "--depth=1", p.cloneURL(), bare)); err != nil {
				return fmt.Errorf("git clone --bare: %w", err)
			}
			return nil
		} else if err != nil {
			return err
		}
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", bare, "fetch", "--depth=1", "--tags", "--force", "origin", "+refs/heads/*:refs/heads/*")); err != nil {
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
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = strings.TrimPrefix(line, "worktree ")
		case line == want && cur != "":
			return cur, true
		}
	}
	return "", false
}

func (p Primary) AddWorktree(ctx context.Context, dest, branch string, run Runner) error {
	if !p.Worktreeable() {
		return fmt.Errorf("service has no git/dir primary; not worktree-able")
	}
	if run == nil {
		return errors.New("source.AddWorktree: nil runner")
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		return nil // existing worktree — reuse
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	gitDir := p.primaryPath()
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
				"remove that worktree first (e.g. `zordon worktree rm`) or point this one elsewhere",
				branch, other)
		}
	}

	if p.Worktree != nil && len(p.Worktree.Sparse) > 0 {
		args := []string{"-C", gitDir, "worktree", "add", "--no-checkout", "--force", "-B", branch, dest, start}
		if err := run(ctx, exec.CommandContext(ctx, "git", args...)); err != nil {
			return fmt.Errorf("git worktree add: %w", err)
		}

		// Initialize sparse checkout
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", dest, "sparse-checkout", "init")); err != nil {
			return fmt.Errorf("git sparse-checkout init: %w", err)
		}

		// Set the specific paths for sparse checkout
		sparseArgs := append([]string{"-C", dest, "sparse-checkout", "set"}, p.Worktree.Sparse...)
		if err := run(ctx, exec.CommandContext(ctx, "git", sparseArgs...)); err != nil {
			return fmt.Errorf("git sparse-checkout set: %w", err)
		}

		// Perform the checkout manually
		if err := run(ctx, exec.CommandContext(ctx, "git", "-C", dest, "checkout", branch)); err != nil {
			return fmt.Errorf("git checkout: %w", err)
		}
	} else {
		args := []string{"-C", gitDir, "worktree", "add", "--force", "-B", branch, dest, start}
		if err := run(ctx, exec.CommandContext(ctx, "git", args...)); err != nil {
			return fmt.Errorf("git worktree add: %w", err)
		}
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
	return os.RemoveAll(dest)
}
