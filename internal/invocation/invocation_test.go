package invocation

import (
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestProjectRootAndWorkspace(t *testing.T) {
	root := t.TempDir()

	pathWS := filepath.Join(root, "workspaces", "feature")
	if err := zfs.EnsureDir(pathWS); err != nil {
		t.Fatal(err)
	}

	// A workspace recognized only by its .workspace marker (parent dir is not
	// literally "workspaces"): the marker is the positive signal.
	markerWS := filepath.Join(root, "elsewhere", "sandbox")
	if err := zfs.EnsureDir(markerWS); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(markerWS, WorkspaceMarker), nil); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		dir      string
		wantRoot string
		wantWS   string
	}{
		"conventional path, no marker":     {dir: pathWS, wantRoot: root, wantWS: "feature"},
		"marker without workspaces parent": {dir: markerWS, wantRoot: root, wantWS: "sandbox"},
		"project root is main":             {dir: root, wantRoot: root, wantWS: MainWorkspace},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			gotRoot, gotWS := projectRootAndWorkspace(c.dir)
			if gotRoot != c.wantRoot || gotWS != c.wantWS {
				t.Fatalf("projectRootAndWorkspace(%q) = (%q, %q); want (%q, %q)",
					c.dir, gotRoot, gotWS, c.wantRoot, c.wantWS)
			}
		})
	}
}

func TestEnclosingCheckout(t *testing.T) {
	root := t.TempDir()

	conv := filepath.Join(root, "workspaces", "wsA", "src", "app")
	if err := zfs.EnsureDir(conv); err != nil {
		t.Fatal(err)
	}
	// A marker workspace two levels below its root, carrying a service checkout.
	marked := filepath.Join(root, "elsewhere", "sandbox")
	if err := zfs.EnsureDir(filepath.Join(marked, "src", "svc")); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(marked, WorkspaceMarker), nil); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		dir      string
		wantOK   bool
		wantRoot string
		wantWS   string
		wantSvc  string
	}{
		"conventional checkout root": {dir: conv, wantOK: true, wantRoot: root, wantWS: "wsA", wantSvc: "app"},
		"deep inside checkout":       {dir: filepath.Join(conv, "cmd", "server"), wantOK: true, wantRoot: root, wantWS: "wsA", wantSvc: "app"},
		"marker workspace checkout":  {dir: filepath.Join(marked, "src", "svc"), wantOK: true, wantRoot: root, wantWS: "sandbox", wantSvc: "svc"},
		"the workspace dir itself":   {dir: filepath.Join(root, "workspaces", "wsA"), wantOK: false},
		"the project root":           {dir: root, wantOK: false},
		"a plain project subdir":     {dir: filepath.Join(root, "src", "app"), wantOK: false},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			gotRoot, gotWS, gotSvc, ok := EnclosingCheckout(c.dir)
			if ok != c.wantOK {
				t.Fatalf("EnclosingCheckout(%q) ok = %v; want %v", c.dir, ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if gotRoot != c.wantRoot || gotWS != c.wantWS || gotSvc != c.wantSvc {
				t.Fatalf("EnclosingCheckout(%q) = (%q, %q, %q); want (%q, %q, %q)",
					c.dir, gotRoot, gotWS, gotSvc, c.wantRoot, c.wantWS, c.wantSvc)
			}
		})
	}
}

// A directory named src that is NOT part of the workspaces/<ws>/src/<svc>
// shape (e.g. a plain <root>/src/<svc>) must not be mistaken for a checkout.
func TestEnclosingCheckout_plainSrcIsNotACheckout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "src", "app")
	if err := zfs.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := EnclosingCheckout(dir); ok {
		t.Fatalf("EnclosingCheckout(%q) reported a checkout; a plain <root>/src/<svc> is not one", dir)
	}
}

// Removing the marker from a conventional workspace must not un-workspace it:
// the path stays authoritative.
func TestProjectRootAndWorkspace_MarkerRemovalKeepsPathWorkspace(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspaces", "feature")
	if err := zfs.EnsureDir(ws); err != nil {
		t.Fatal(err)
	}
	// no marker written on purpose
	if gotRoot, gotWS := projectRootAndWorkspace(ws); gotRoot != root || gotWS != "feature" {
		t.Fatalf("projectRootAndWorkspace(%q) = (%q, %q); want (%q, %q)", ws, gotRoot, gotWS, root, "feature")
	}
}
