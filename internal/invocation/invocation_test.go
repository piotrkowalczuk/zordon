package invocation

import (
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// ResolveInvocation is the single source of truth for "from this cwd, what does
// zordon run?" — it walks up to the nearest workspace boundary (a
// workspaces/<name> dir or a .workspace marker), then to the project Alphasfile
// at or above that boundary. The cases below pin every branch that matters,
// including the two the issue #73 rework is about: a subdir resolves to the
// enclosing stack (not a nested one), and a marker boundary SHADOWS an
// Alphasfile buried in a service checkout below it.
func TestResolveInvocation(t *testing.T) {
	root := t.TempDir()
	writeAlphasfile(t, root)

	// A conventional workspace with a service checkout that carries its own
	// Alphasfile (a service repo does), plus a marker on the workspace dir.
	convWS := filepath.Join(root, "workspaces", "feature")
	mkMarkedWorkspace(t, convWS)
	buriedCheckout := filepath.Join(convWS, "src", "app")
	writeAlphasfile(t, buriedCheckout)

	// A marker workspace living OUTSIDE workspaces/ (marker is the only signal),
	// two levels below the root.
	markerWS := filepath.Join(root, "elsewhere", "sandbox")
	mkMarkedWorkspace(t, markerWS)

	cases := map[string]struct {
		cwd            string
		wantRoot       string
		wantWS         string
		wantInvocation string
	}{
		"project root → main": {
			cwd: root, wantRoot: root, wantWS: MainWorkspace, wantInvocation: root,
		},
		"plain project subdir → main (walk up)": {
			cwd: filepath.Join(root, "src", "app"), wantRoot: root, wantWS: MainWorkspace, wantInvocation: root,
		},
		"workspace dir → that workspace": {
			cwd: convWS, wantRoot: root, wantWS: "feature", wantInvocation: convWS,
		},
		"inside a checkout, buried Alphasfile SHADOWED by marker": {
			cwd: buriedCheckout, wantRoot: root, wantWS: "feature", wantInvocation: convWS,
		},
		"deep inside a checkout → the workspace": {
			cwd: filepath.Join(buriedCheckout, "cmd", "server"), wantRoot: root, wantWS: "feature", wantInvocation: convWS,
		},
		"marker workspace outside workspaces/": {
			cwd: markerWS, wantRoot: root, wantWS: "sandbox", wantInvocation: markerWS,
		},
		"deep inside a marker workspace": {
			cwd: filepath.Join(markerWS, "src", "svc", "pkg"), wantRoot: root, wantWS: "sandbox", wantInvocation: markerWS,
		},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			gotRoot, gotWS, gotInv, err := ResolveInvocation(c.cwd)
			if err != nil {
				t.Fatalf("ResolveInvocation(%q): %v", c.cwd, err)
			}
			if gotRoot != c.wantRoot || gotWS != c.wantWS || gotInv != c.wantInvocation {
				t.Fatalf("ResolveInvocation(%q) = (root=%q, ws=%q, inv=%q); want (root=%q, ws=%q, inv=%q)",
					c.cwd, gotRoot, gotWS, gotInv, c.wantRoot, c.wantWS, c.wantInvocation)
			}
		})
	}
}

// The .workspace marker is authoritative on the WHOLE subtree, not just the dir
// it sits in: from a deep subdir with no marker of its own, resolution still
// finds the ancestor marker and runs that workspace.
func TestResolveInvocation_markerIsLoadBearingUpTheTree(t *testing.T) {
	root := t.TempDir()
	writeAlphasfile(t, root)
	ws := filepath.Join(root, "elsewhere", "sandbox") // NOT under workspaces/
	mkMarkedWorkspace(t, ws)

	deep := filepath.Join(ws, "a", "b", "c")
	gotRoot, gotWS, gotInv, err := ResolveInvocation(deep)
	if err != nil {
		t.Fatal(err)
	}
	if gotWS != "sandbox" || gotRoot != root || gotInv != ws {
		t.Fatalf("deep marker subtree: got (root=%q ws=%q inv=%q); want (root=%q ws=sandbox inv=%q)",
			gotRoot, gotWS, gotInv, root, ws)
	}
}

// Two runs from different subdirs of the same project resolve to the SAME
// invocation dir (hence the same FsHash / StateDir) — the property that stops a
// subdir from forking its own nested stack (issue #73). A distinct workspace
// resolves to a distinct invocation dir.
func TestResolveInvocation_stableAcrossSubdirs(t *testing.T) {
	root := t.TempDir()
	writeAlphasfile(t, root)
	ws := filepath.Join(root, "workspaces", "feature")
	mkMarkedWorkspace(t, ws)

	a := mustInvocationDir(t, filepath.Join(root, "src", "app"))
	b := mustInvocationDir(t, filepath.Join(root, "cmd", "tool", "deep"))
	if a != root || b != root {
		t.Fatalf("main subdirs must resolve to root %q; got a=%q b=%q", root, a, b)
	}
	w1 := mustInvocationDir(t, ws)
	w2 := mustInvocationDir(t, filepath.Join(ws, "src", "svc"))
	if w1 != ws || w2 != ws {
		t.Fatalf("workspace subdirs must resolve to %q; got w1=%q w2=%q", ws, w1, w2)
	}
	if a == w1 {
		t.Fatal("main and a named workspace must NOT share an invocation dir")
	}
}

// No Alphasfile anywhere up the tree is a clean error, not a panic.
func TestResolveInvocation_noAlphasfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	if err := zfs.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ResolveInvocation(dir); err == nil {
		t.Fatal("want error when no Alphasfile exists at or above cwd")
	}
}

func mustInvocationDir(t *testing.T, cwd string) string {
	t.Helper()
	_, _, inv, err := ResolveInvocation(cwd)
	if err != nil {
		t.Fatalf("ResolveInvocation(%q): %v", cwd, err)
	}
	return inv
}

func writeAlphasfile(t *testing.T, dir string) {
	t.Helper()
	if err := zfs.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(dir, AlphasfileName), []byte("\n")); err != nil {
		t.Fatal(err)
	}
}

func mkMarkedWorkspace(t *testing.T, dir string) {
	t.Helper()
	if err := zfs.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(dir, WorkspaceMarker), nil); err != nil {
		t.Fatal(err)
	}
}
