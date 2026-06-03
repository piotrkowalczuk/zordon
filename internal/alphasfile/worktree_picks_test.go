package alphasfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// TestWorktreePartialPick_NonPickedFallsBackToAnchor pins the rule that
// services NOT picked at `zordon worktree create <wt> [svc ...]` resolve
// their dir to the anchor Alphasfile's project root (same place
// main-worktree InPlace puts them), NOT to <wtdir>/src/<svc>. The
// "owned" set is determined from disk: presence of <wtdir>/src/<svc>/.git
// (left by `git worktree add` in cmd/zordon/worktree.go:checkoutServices).
//
// Today the resolver overrides every locally-declared service in a named
// worktree to inv.CheckoutPath(name) — so non-picked services point at an
// empty/forked dir under the worktree, and provision shells with relative
// paths (`./plik.sql`) run with the wrong cwd. This test is expected to
// fail until that override is fixed.
func TestWorktreePartialPick_NonPickedFallsBackToAnchor(t *testing.T) {
	tmp := t.TempDir()
	af := []byte(`
service "go" "serviceA" {
  src {
    path = "."
    exe  = "./a"
  }
  runtime { cmd = ["./a"] }
}
service "go" "serviceB" {
  src {
    path = "."
    exe  = "./b"
  }
  runtime { cmd = ["./b"] }
}
service "go" "serviceC" {
  src {
    path = "."
    exe  = "./c"
  }
  runtime { cmd = ["./c"] }
}
`)
	afPath := filepath.Join(tmp, "Alphasfile")
	if err := os.WriteFile(afPath, af, 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate `zordon worktree create feature serviceA`: only serviceA
	// gets a checkout (we leave a .git marker, mimicking what
	// `git worktree add` leaves behind).
	wtDir := filepath.Join(tmp, ".zordon", "worktrees", "feature")
	svcADir := filepath.Join(wtDir, "src", "serviceA")
	if err := os.MkdirAll(svcADir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svcADir, ".git"), []byte("gitdir: irrelevant\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	inv, err := invocation.New(wtDir, af, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Worktree != "feature" {
		t.Fatalf("invocation worktree = %q, want %q (wtDir %s not recognised as a worktree)",
			inv.Worktree, "feature", wtDir)
	}

	got, err := Compile(afPath, af, inv, nil, TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Runtime.Dir is the exe-anchored work dir (zfs.ServiceCwd): the
	// checkout root joined with src.exe. Owned services anchor on the
	// per-worktree checkout; non-picked services anchor on the
	// (in-place) project root. Both join the service's `exe` offset.
	want := map[string]string{
		"serviceA": filepath.Join(svcADir, "a"), // owned: <wtdir>/src/serviceA + ./a
		"serviceB": filepath.Join(tmp, "b"),     // not owned: <project> + ./b
		"serviceC": filepath.Join(tmp, "c"),     // not owned: <project> + ./c
	}
	for name, wantDir := range want {
		s := svcByName(got, name)
		if s == nil || s.Runtime == nil {
			t.Errorf("%s: no resolved runtime", name)
			continue
		}
		if s.Runtime.Dir != wantDir {
			t.Errorf("%s.Runtime.Dir = %q, want %q", name, s.Runtime.Dir, wantDir)
		}
	}
}
