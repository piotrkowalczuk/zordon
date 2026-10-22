package invocation

import (
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// A module service checks out one level deeper (src/<module>/<svc>), so the
// ownership scan must recognize both the flat and the nested marker layout.
func TestInvocationState_OwnsService_nestedModuleCheckout(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces", "feature")
	for _, rel := range []string{"api", "payments/db"} {
		if err := zfs.EnsureDir(filepath.Join(ws, "src", rel, ".git")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zfs.EnsureDir(filepath.Join(ws, "src", "payments", "api")); err != nil {
		t.Fatal(err)
	}
	inv := build(ws, root, "feature")

	cases := map[string]struct {
		svc  string
		want bool
	}{
		"flat checkout":               {"api", true},
		"nested module checkout":      {"payments/db", true},
		"nested dir without .git":     {"payments/api", false},
		"module dir is not a service": {"payments", false},
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

// A package's services check out at src/<package>/<component>/<svc>. When that
// is the only layout on disk, the scan must still find them: missing them leaves
// ownership unknown, which owns every service and forks a checkout of each.
func TestInvocationState_OwnsService_componentCheckout(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces", "feature")
	if err := zfs.EnsureDir(filepath.Join(ws, "src", "billing", "app", "billing", ".git")); err != nil {
		t.Fatal(err)
	}
	inv := build(ws, root, "feature")

	cases := map[string]struct {
		svc  string
		want bool
	}{
		"component-checkout":             {"billing/app/billing", true},
		"unpicked-service":               {"ledger/app/ledger", false},
		"component-dir-is-not-a-service": {"billing/app", false},
		"package-dir-is-not-a-service":   {"billing", false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := inv.OwnsService(c.svc); got != c.want {
				t.Errorf("OwnsService(%q) = %v, want %v", c.svc, got, c.want)
			}
		})
	}
}
