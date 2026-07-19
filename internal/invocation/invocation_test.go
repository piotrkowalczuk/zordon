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
