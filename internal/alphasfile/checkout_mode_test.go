package alphasfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// The resolver tags each service's Package with how alpha should materialize
// it: InPlace (build from the live src tree), Editable (its own git worktree on
// zordon/<ws>/<svc>), or neither (a plain clone at ref). The four cases from
// issue #73's design must map to exactly the right flags — in particular a
// git-source service that was NOT picked is a plain clone, never a worktree.
func TestResolve_checkoutMode(t *testing.T) {
	tmp := t.TempDir()
	af := []byte(`
service "go" "local" {
  src {
    path = "."
    exe  = "./local"
  }
  runtime { cmd = ["./local"] }
}
service "go" "thirdparty" {
  git {
    url = "github.com/acme/thirdparty"
  }
  runtime { cmd = ["./tp"] }
}
`)
	afPath := filepath.Join(tmp, "Alphasfile")
	if err := os.WriteFile(afPath, af, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("main workspace", func(t *testing.T) {
		inv, err := invocation.NewInvocationState(tmp)
		if err != nil {
			t.Fatal(err)
		}
		got := mustCompile(t, afPath, af, inv)
		// case 2: local src, not picked (main never picks) → in place.
		assertMode(t, got, "local", pkgMode{inPlace: true})
		// case 3: git source, not picked → plain clone (no branch, no worktree).
		assertMode(t, got, "thirdparty", pkgMode{})
	})

	t.Run("named workspace, thirdparty picked", func(t *testing.T) {
		// Simulate `zordon workspace create feature thirdparty`: only the git
		// service gets a checkout (.git marker = an editable worktree exists).
		wtDir := filepath.Join(tmp, "workspaces", "feature")
		tpDir := filepath.Join(wtDir, "src", "thirdparty")
		if err := os.MkdirAll(tpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tpDir, ".git"), []byte("gitdir: irrelevant\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		inv, err := invocation.NewInvocationState(wtDir)
		if err != nil {
			t.Fatal(err)
		}
		got := mustCompile(t, afPath, af, inv)
		// case 1: picked git source → editable worktree.
		assertMode(t, got, "thirdparty", pkgMode{editable: true})
		// case 2 still: local src not picked → in place.
		assertMode(t, got, "local", pkgMode{inPlace: true})
	})
}

type pkgMode struct {
	inPlace  bool
	editable bool
}

func mustCompile(t *testing.T, afPath string, af []byte, inv *invocation.InvocationState) *Alphasfile {
	t.Helper()
	got, err := Compile(afPath, af, inv, nil, invocation.ConfigHash(af, nil), TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return got
}

func assertMode(t *testing.T, af *Alphasfile, name string, want pkgMode) {
	t.Helper()
	s := svcByName(af, name)
	if s == nil || s.Package == nil {
		t.Fatalf("%s: no resolved package", name)
	}
	if s.Package.InPlace != want.inPlace || s.Package.Editable != want.editable {
		t.Errorf("%s: InPlace=%v Editable=%v; want InPlace=%v Editable=%v",
			name, s.Package.InPlace, s.Package.Editable, want.inPlace, want.editable)
	}
}
