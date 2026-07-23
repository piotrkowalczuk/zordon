package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// gateInvocationDir is the invocation gate walkUp applies after finding an
// Alphasfile: zordon runs only from the project root or a workspace dir. A run
// from a plain subdir, or from inside a service checkout that carries its own
// Alphasfile (issue #73), is refused.
func TestGateInvocationDir(t *testing.T) {
	root := t.TempDir()
	writeAlphasfile(t, root)
	if err := zfs.EnsureDir(filepath.Join(root, "src", "app")); err != nil {
		t.Fatal(err)
	}
	if err := zfs.EnsureDir(filepath.Join(root, "workspaces", "wsA")); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		cwd     string
		wantErr string // substring; "" ⇒ no error
	}{
		"project root":  {cwd: root},
		"workspace dir": {cwd: filepath.Join(root, "workspaces", "wsA")},
		"plain subdir":  {cwd: filepath.Join(root, "src", "app"), wantErr: "not a zordon invocation dir"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			err := gateInvocationDir(c.cwd, root)
			assertGateErr(t, err, c.wantErr)
		})
	}
}

// An Alphasfile physically inside <outer>/workspaces/<ws>/src/<svc> is source
// carried by the service repo — adopting it as a leaf roots a nested stack.
// The gate refuses it and names the outer project's two invocation dirs.
func TestGateInvocationDir_buriedAlphasfileRefused(t *testing.T) {
	outer := t.TempDir()
	writeAlphasfile(t, outer)
	checkout := filepath.Join(outer, "workspaces", "wsA", "src", "app")
	if err := zfs.EnsureDir(checkout); err != nil {
		t.Fatal(err)
	}
	writeAlphasfile(t, checkout)

	err := gateInvocationDir(checkout, checkout)
	assertGateErr(t, err, "managed checkout cannot be a project root")
	if err != nil && !strings.Contains(err.Error(), `workspace "wsA"`) {
		t.Fatalf("error should name the workspace; got: %v", err)
	}
}

// A checkout whose OUTER project has no Alphasfile is not a zordon layout, so
// the specific "buried checkout" message must not fire — the buried Alphasfile
// stands on its own as the root.
func TestGateInvocationDir_checkoutWithoutOuterProjectIsRoot(t *testing.T) {
	base := t.TempDir()
	checkout := filepath.Join(base, "workspaces", "wsA", "src", "app")
	if err := zfs.EnsureDir(checkout); err != nil {
		t.Fatal(err)
	}
	// No Alphasfile at base — only in the checkout.
	writeAlphasfile(t, checkout)
	if err := gateInvocationDir(checkout, checkout); err != nil {
		t.Fatalf("checkout with no outer project should be a valid root; got: %v", err)
	}
}

func writeAlphasfile(t *testing.T, dir string) {
	t.Helper()
	if err := zfs.AtomicWrite(filepath.Join(dir, "Alphasfile"), []byte("\n")); err != nil {
		t.Fatal(err)
	}
}

func assertGateErr(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("want no error; got: %v", err)
	case want != "" && err == nil:
		t.Fatalf("want error containing %q; got nil", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("want error containing %q; got: %v", want, err)
	}
}
