package source

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// A third-party (unpicked) git-source service is materialized by a plain clone
// checked out DETACHED at its ref: correct content, no zordon/<ws>/<svc>
// branch, and — crucially for issue #73 — no worktree registration in the
// source repo, so two workspaces cloning the same service never contend for an
// admin dir.
func TestPrimary_Clone_materializesDetachedAtRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := taggedRepo(t)
	p := mustDirPrimary(t, repo, "v1")

	dest := filepath.Join(t.TempDir(), "src", "app")
	if err := p.Clone(t.Context(), dest, "v1", run); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	assertFileContains(t, filepath.Join(dest, "README.md"), "one")
	if got := headRef(t, dest); got != "HEAD" {
		t.Errorf("clone should be detached; HEAD symbolic ref = %q, want detached (\"HEAD\")", got)
	}
	// No branch, and no worktree registered against the source repo.
	if list := worktreeList(t, repo); strings.Contains(list, dest) {
		t.Errorf("clone must not register a worktree in the source repo; `worktree list`:\n%s", list)
	}
}

// A finished clone at the same ref is reused as-is: a sentinel file dropped
// into the working tree survives (a rebuild would wipe it).
func TestPrimary_Clone_reusesUnchangedRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	p := mustDirPrimary(t, taggedRepo(t), "v1")
	dest := filepath.Join(t.TempDir(), "src", "app")
	if err := p.Clone(t.Context(), dest, "v1", run); err != nil {
		t.Fatalf("first Clone: %v", err)
	}
	sentinel := filepath.Join(dest, "sentinel")
	if err := zfs.AtomicWrite(sentinel, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := p.Clone(t.Context(), dest, "v1", run); err != nil {
		t.Fatalf("second Clone: %v", err)
	}
	if !zfs.Exists(sentinel) {
		t.Fatal("unchanged-ref clone was rebuilt, not reused (sentinel gone)")
	}
}

// A changed ref rebuilds the tree so the working copy follows the new ref.
func TestPrimary_Clone_rebuildsOnChangedRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := taggedRepo(t)
	dest := filepath.Join(t.TempDir(), "src", "app")

	if err := mustDirPrimary(t, repo, "v1").Clone(t.Context(), dest, "v1", run); err != nil {
		t.Fatalf("Clone v1: %v", err)
	}
	assertFileContains(t, filepath.Join(dest, "README.md"), "one")
	if err := mustDirPrimary(t, repo, "v2").Clone(t.Context(), dest, "v2", run); err != nil {
		t.Fatalf("Clone v2: %v", err)
	}
	assertFileContains(t, filepath.Join(dest, "README.md"), "two")
}

// Migration: an existing checkout left by an older zordon is a registered
// worktree on zordon/<ws>/<svc>. Clone must de-register it (not just delete the
// dir) so the branch is freed and the source repo no longer lists a worktree
// there — otherwise a later `workspace create` of the same service would hit
// "branch already checked out".
func TestPrimary_Clone_migratesFromRegisteredWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := taggedRepo(t)
	p := mustDirPrimary(t, repo, "v1")
	dest := filepath.Join(t.TempDir(), "src", "app")

	// Old-world state: a real worktree on zordon/main/app at dest.
	if _, err := p.AddWorktree(t.Context(), dest, "zordon/main/app", run); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	if list := worktreeList(t, repo); !strings.Contains(list, dest) {
		t.Fatalf("precondition: worktree not registered:\n%s", list)
	}

	if err := p.Clone(t.Context(), dest, "v1", run); err != nil {
		t.Fatalf("Clone over registered worktree: %v", err)
	}
	if list := worktreeList(t, repo); strings.Contains(list, dest) {
		t.Errorf("migration left a registered worktree behind:\n%s", list)
	}
	// The freed branch can be re-added elsewhere without a collision.
	if _, err := p.AddWorktree(t.Context(), filepath.Join(t.TempDir(), "again"), "zordon/main/app", run); err != nil {
		t.Errorf("branch not freed by migration: %v", err)
	}
}

// worktreeHealthy is the reuse gate: it must accept only a complete, correctly
// registered worktree on the expected branch, and reject the corruptions that
// prune/re-add churn produces (issue #73 comment) — a cross-linked back-pointer
// or a checkout silently sitting on another workspace's branch.
func TestWorktreeHealthy(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	branch := "zordon/wsA/app"

	t.Run("complete worktree is healthy", func(t *testing.T) {
		dest := freshWorktree(t, branch)
		if !worktreeHealthy(t.Context(), dest, branch) {
			t.Fatal("a freshly added, unlocked worktree on its branch must be healthy")
		}
	})

	t.Run("wrong branch is not healthy", func(t *testing.T) {
		dest := freshWorktree(t, branch)
		if worktreeHealthy(t.Context(), dest, "zordon/wsB/app") {
			t.Fatal("a worktree on wsA's branch must not read as healthy for wsB's branch")
		}
	})

	t.Run("cross-linked back-pointer is not healthy", func(t *testing.T) {
		dest := freshWorktree(t, branch)
		admin := adminDir(t, dest)
		// Point the admin dir's gitdir back-pointer at a DIFFERENT working dir,
		// exactly the disagreement a reassigned admin basename leaves behind.
		if err := zfs.AtomicWrite(filepath.Join(admin, "gitdir"),
			[]byte(filepath.Join(t.TempDir(), "elsewhere", ".git")+"\n")); err != nil {
			t.Fatal(err)
		}
		if worktreeHealthy(t.Context(), dest, branch) {
			t.Fatal("a worktree whose admin gitdir names another tree must not read as healthy")
		}
	})

	t.Run("unreadable gitlink is not healthy", func(t *testing.T) {
		dest := freshWorktree(t, branch)
		if err := zfs.AtomicWrite(filepath.Join(dest, ".git"),
			[]byte("gitdir: /nonexistent/admin\n")); err != nil {
			t.Fatal(err)
		}
		if worktreeHealthy(t.Context(), dest, branch) {
			t.Fatal("a worktree whose .git dangles must not read as healthy (the old bare-exists bug)")
		}
	})
}

func mustDirPrimary(t *testing.T, repo, ref string) Primary {
	t.Helper()
	p, err := NewPrimary("", "", repo, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// taggedRepo is a repo with tags v1 and v2 whose README.md reads "one" then
// "two" — enough to tell which ref a checkout landed on.
func taggedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	writeCommitTag(t, repo, "one", "v1")
	writeCommitTag(t, repo, "two", "v2")
	return repo
}

func writeCommitTag(t *testing.T, repo, body, tag string) {
	t.Helper()
	if err := zfs.AtomicWrite(filepath.Join(repo, "README.md"), []byte(body+"\n")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "add", "-A")
	git(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "commit", "-q", "-m", body)
	git(t, repo, "tag", tag)
}

// freshWorktree adds a completed worktree on branch and returns its dir.
func freshWorktree(t *testing.T, branch string) string {
	t.Helper()
	p := mustDirPrimary(t, commitFileRepo(t, "pkg/app/main.go", "package app\n"), "")
	dest := filepath.Join(t.TempDir(), "src", "app")
	if _, err := p.AddWorktree(t.Context(), dest, branch, run); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	return dest
}

func adminDir(t *testing.T, dest string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dest, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatalf("rev-parse admin dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// headRef returns the symbolic ref of HEAD, or "HEAD" when detached.
func headRef(t *testing.T, dest string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dest, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func worktreeList(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	return string(out)
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := zfs.Read(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Fatalf("%s = %q; want it to contain %q", path, string(b), want)
	}
}
