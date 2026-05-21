package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	ctx := context.Background()
	wt := t.TempDir()

	// Two services off the SAME primary, distinct per-service branches.
	for _, svc := range []string{"svc-a", "svc-b"} {
		dest := filepath.Join(wt, "src", svc)
		if err := p.AddWorktree(ctx, dest, "zordon/main/"+svc, run); err != nil {
			t.Fatalf("monorepo worktree %s: %v", svc, err)
		}
		if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
			t.Fatalf("worktree %s not materialized: %v", svc, err)
		}
	}

	// Same branch reused at a different path still errors clearly (the
	// guard must remain — it's now per-service, not per-worktree).
	err = p.AddWorktree(ctx, filepath.Join(wt, "elsewhere"), "zordon/main/svc-a", run)
	if err == nil || !strings.Contains(err.Error(), "already checked out") {
		t.Fatalf("want already-checked-out error for reused branch, got %v", err)
	}
}
