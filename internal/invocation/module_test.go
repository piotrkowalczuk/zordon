package invocation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// A module service checks out one level deeper (src/<module>/<svc>) and a
// component's service two (src/<package>/<component>/<svc>), so the
// ownership scan must recognize every marker layout a display name has.
func TestInvocationState_OwnsService_nestedModuleCheckout(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces", "feature")
	for _, rel := range []string{"api", "payments/db", "risk-engine/app/risk-engine", "legacy/app/svc/four"} {
		writeGitlink(t, filepath.Join(ws, "src", rel))
	}
	for _, rel := range []string{"payments/api", "vendor/clone/.git"} {
		if err := zfs.EnsureDir(filepath.Join(ws, "src", rel)); err != nil {
			t.Fatal(err)
		}
	}
	inv := build(ws, root, "feature")

	cases := map[string]struct {
		svc  string
		want bool
	}{
		"flat checkout":                     {"api", true},
		"nested module checkout":            {"payments/db", true},
		"nested dir without .git":           {"payments/api", false},
		"module dir is not a service":       {"payments", false},
		"component checkout":                {"risk-engine/app/risk-engine", true},
		"component dir is not a service":    {"risk-engine/app", false},
		"four levels is not a display name": {"legacy/app/svc/four", false},
		"plain clone of an unpicked source": {"vendor/clone", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := inv.OwnsService(c.svc); got != c.want {
				t.Errorf("OwnsService(%q) = %v, want %v", c.svc, got, c.want)
			}
		})
	}
	if got := inv.CheckoutPath("payments/db"); got != filepath.Join(ws, "src", "payments", "db") {
		t.Errorf("CheckoutPath = %q", got)
	}
}

// writeGitlink leaves at dir what `git worktree add` does: a .git file.
func writeGitlink(t *testing.T, dir string) {
	t.Helper()
	if err := zfs.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
