package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// ensureAnchorDirs pre-creates each service's fs::etc / fs::var dirs (0o750) so
// a provision writing into ${fs::var()}/... doesn't have to mkdir first.
// Services with no anchor paths (or no runtime) are skipped without error.
func TestEnsureAnchorDirs_createsEtcVarAt0750(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "workspaces", "main", "etc", "api")
	vardir := filepath.Join(root, "workspaces", "main", "var", "api")

	services := []*alphasfile.Service{
		{Runtime: &alphasfile.RuntimeConfig{Name: "api", EtcDir: etc, VarDir: vardir}},
		{Runtime: &alphasfile.RuntimeConfig{Name: "noanchor"}}, // empty dirs → skipped
		{Runtime: nil}, // nil runtime → skipped
	}

	if err := ensureAnchorDirs(services, discardLog(t)); err != nil {
		t.Fatalf("ensureAnchorDirs: %v", err)
	}

	for _, dir := range []string{etc, vardir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("anchor dir %s not created: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
		if perm := info.Mode().Perm(); perm != 0o750 {
			t.Errorf("%s mode = %o, want 0750", dir, perm)
		}
	}
}
