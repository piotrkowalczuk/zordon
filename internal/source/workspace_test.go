package source

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func run(ctx context.Context, c *exec.Cmd) error { return c.Run() }

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// Monorepo: several services share one primary repo. Each must get its
// own per-service branch (zordon/<wt>/<svc>); otherwise the 2nd
// `git worktree add` fails with "branch already checked out".
func TestMonorepoPerServiceBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "commit", "-q",
		"--allow-empty", "-m", "init")

	p, err := NewPrimary("", "", repo, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindDir {
		t.Fatalf("kind=%q", p.Kind)
	}
	ctx := t.Context()
	wt := t.TempDir()

	// Two services off the SAME primary, distinct per-service branches.
	for _, svc := range []string{"svc-a", "svc-b"} {
		dest := filepath.Join(wt, "src", svc)
		if _, err := p.AddWorktree(ctx, dest, "zordon/main/"+svc, run); err != nil {
			t.Fatalf("monorepo workspace %s: %v", svc, err)
		}
		if !zfs.Exists(filepath.Join(dest, ".git")) {
			t.Fatalf("workspace %s not materialized", svc)
		}
	}

	// Same branch reused at a different path still errors clearly (the
	// guard must remain — it's now per-service, not per-workspace).
	_, err = p.AddWorktree(ctx, filepath.Join(wt, "elsewhere"), "zordon/main/svc-a", run)
	if err == nil || !strings.Contains(err.Error(), "already checked out") {
		t.Fatalf("want already-checked-out error for reused branch, got %v", err)
	}
}

// A checkout the user has taken onto a different branch (with uncommitted work)
// must be REUSED as-is, never force-rebuilt: zordon warns and builds what is
// there instead of resetting the branch and deleting the tree. Losing a
// developer's work to a start is the outcome this guards against.
func TestPrimary_AddWorktree_respectsUserBranchSwitch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := commitFileRepo(t, "app/main.go", "package app\n")
	git(t, repo, "branch", "mine")
	p, err := NewPrimary("", "", repo, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	dest := filepath.Join(t.TempDir(), "src", "app")
	branch := "zordon/feature/app"
	if _, err := p.AddWorktree(ctx, dest, branch, run); err != nil {
		t.Fatalf("first AddWorktree: %v", err)
	}

	// User switches the checkout to their own branch and edits without committing.
	git(t, dest, "checkout", "-q", "mine")
	edit := filepath.Join(dest, "app", "main.go")
	if err := zfs.AtomicWrite(edit, []byte("package app // WIP\n")); err != nil {
		t.Fatal(err)
	}

	warn, err := p.AddWorktree(ctx, dest, branch, run)
	if err != nil {
		t.Fatalf("second AddWorktree must not error over the user's checkout: %v", err)
	}
	if warn == "" || !strings.Contains(warn, "mine") {
		t.Errorf("want a warning naming branch %q; got %q", "mine", warn)
	}
	// The user's branch and uncommitted edit survive untouched.
	if got := currentBranch(t, dest); got != "mine" {
		t.Errorf("checkout was reset to %q; user's branch %q must be preserved", got, "mine")
	}
	if b, _ := zfs.Read(edit); !strings.Contains(string(b), "WIP") {
		t.Error("uncommitted edit was lost — AddWorktree overwrote the user's tree")
	}
}

// Content that is not a git worktree at all (a plain directory the user
// populated) is likewise reused with a warning, never wiped.
func TestPrimary_AddWorktree_respectsPlainDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	p, err := NewPrimary("", "", commitFileRepo(t, "app/main.go", "package app\n"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "src", "app")
	if err := zfs.EnsureDir(dest); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dest, "notes.txt")
	if err := zfs.AtomicWrite(keep, []byte("my notes\n")); err != nil {
		t.Fatal(err)
	}
	warn, err := p.AddWorktree(t.Context(), dest, "zordon/feature/app", run)
	if err != nil {
		t.Fatalf("AddWorktree over a plain dir must not error: %v", err)
	}
	if warn == "" {
		t.Error("want a warning that existing content was reused as-is")
	}
	if !zfs.Exists(keep) {
		t.Error("plain-directory content was deleted — must be preserved")
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// failfast can kill a service mid-bringup. `git worktree add` writes
// dest/.git *before* the sparse init/set/checkout finish, so a kill in
// that window strands a worktree with a .git but no checked-out sources.
// The next AddWorktree must rebuild it, not silently reuse the broken
// tree on the bare existence of .git (issue #38).
func TestPrimary_AddWorktree_rebuildsInterruptedSparseCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := commitFileRepo(t, "pkg/app/main.go", "package app\n")

	p, err := NewPrimary("", "", repo, "", &Workspace{Sparse: []string{"pkg/app"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	dest := filepath.Join(t.TempDir(), "src", "app")
	src := filepath.Join(dest, "pkg", "app", "main.go")

	// Run every git step for real except the first command after
	// `worktree add` (sparse-checkout init), which we drop to mimic a
	// failfast kill: dest/.git is written, but no source is checked out.
	killed := func(ctx context.Context, c *exec.Cmd) error {
		if slices.Contains(c.Args, "sparse-checkout") && slices.Contains(c.Args, "init") {
			return errors.New("simulated failfast kill mid-setup")
		}
		return c.Run()
	}
	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", killed); err == nil {
		t.Fatal("interrupted setup: want error from AddWorktree, got nil")
	}
	if !zfs.Exists(filepath.Join(dest, ".git")) {
		t.Fatal("interrupted setup should still leave dest/.git")
	}
	if !zfs.Missing(src) {
		t.Fatalf("interrupted setup should leave source absent: %s", src)
	}

	// Next start must heal the half-initialized tree and materialize the
	// source — not return nil on the bare existence of dest/.git.
	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", run); err != nil {
		t.Fatalf("second AddWorktree: %v", err)
	}
	if !zfs.Exists(src) {
		t.Fatalf("worktree not healed: %s still missing after reuse", src)
	}
}

// A worktree whose setup finished must be unlocked (so the next start
// reuses it) and reuse must be a no-op — not a rebuild that re-clones and
// re-installs every start. A sentinel dropped into the tree survives reuse
// but would vanish on a rebuild.
func TestPrimary_AddWorktree_reusesCompletedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := commitFileRepo(t, "pkg/app/main.go", "package app\n")
	p, err := NewPrimary("", "", repo, "", &Workspace{Sparse: []string{"pkg/app"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	dest := filepath.Join(t.TempDir(), "src", "app")

	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", run); err != nil {
		t.Fatalf("first AddWorktree: %v", err)
	}
	if !worktreeHealthy(ctx, dest, "zordon/main/app") {
		t.Fatal("completed worktree is not healthy (should be unlocked and on its branch)")
	}

	sentinel := filepath.Join(dest, "sentinel")
	if err := zfs.AtomicWrite(sentinel, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", run); err != nil {
		t.Fatalf("second AddWorktree: %v", err)
	}
	if !zfs.Exists(sentinel) {
		t.Fatal("healthy worktree was rebuilt, not reused (sentinel gone)")
	}
}

// Self-heal keys off the lock itself, not a reason (`git worktree add`
// can't set a reason portably across CI's older gits). A finished worktree
// is unlocked, so any worktree found locked is an unfinished setup and must
// be rebuilt — a sentinel dropped into it does not survive the rebuild.
func TestPrimary_AddWorktree_rebuildsLockedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := commitFileRepo(t, "pkg/app/main.go", "package app\n")
	p, err := NewPrimary("", "", repo, "", &Workspace{Sparse: []string{"pkg/app"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	dest := filepath.Join(t.TempDir(), "src", "app")

	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", run); err != nil {
		t.Fatalf("first AddWorktree: %v", err)
	}
	sentinel := filepath.Join(dest, "sentinel")
	if err := zfs.AtomicWrite(sentinel, []byte("x")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "worktree", "lock", dest)

	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", run); err != nil {
		t.Fatalf("second AddWorktree: %v", err)
	}
	if zfs.Exists(sentinel) {
		t.Fatal("locked worktree was reused, not rebuilt (sentinel survived)")
	}
	if !zfs.Exists(filepath.Join(dest, "pkg", "app", "main.go")) {
		t.Fatal("rebuilt worktree is missing its source")
	}
}

// A setup killed mid-init can leave the worktree LOCKED in the bare's admin
// registration while the working dir is later cleaned away (#40). The
// dangling locked registration keeps holding <branch> — plain `worktree
// prune` skips locked entries — so the next AddWorktree would fail with
// "cannot force update the branch … used by worktree at <dest>". It must
// instead reclaim the orphan (unlock + prune) and rebuild.
func TestPrimary_AddWorktree_reclaimsOrphanedLockedRegistration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := commitFileRepo(t, "pkg/app/main.go", "package app\n")
	p, err := NewPrimary("", "", repo, "", &Workspace{Sparse: []string{"pkg/app"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	dest := filepath.Join(t.TempDir(), "app")
	src := filepath.Join(dest, "pkg", "app", "main.go")

	// Register the branch as a locked worktree, then delete the dir —
	// leaving the orphaned, locked registration behind.
	// `--lock` (no `--reason`) mirrors AddWorktree's own invocation: the lock
	// is the sentinel, and `git worktree add --reason` only exists in git 2.34+
	// which the debian:11 CI leg (git 2.30) rejects.
	git(t, repo, "worktree", "add", "--lock",
		"--no-checkout", "--force", "-B", "zordon/main/app", dest, "HEAD")
	if err := zfs.RemoveTree(dest); err != nil {
		t.Fatal(err)
	}

	if _, err := p.AddWorktree(ctx, dest, "zordon/main/app", run); err != nil {
		t.Fatalf("AddWorktree over orphaned locked registration: %v", err)
	}
	if !zfs.Exists(src) {
		t.Fatalf("worktree not rebuilt: %s missing", src)
	}
}

// commitFileRepo creates a temp git repo with a single committed file at
// rel and returns the repo dir.
func commitFileRepo(t *testing.T, rel, content string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	full := filepath.Join(repo, rel)
	if err := zfs.EnsureDir(filepath.Dir(full)); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(full, []byte(content)); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "add", "-A")
	git(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "commit", "-q", "-m", "init")
	return repo
}
