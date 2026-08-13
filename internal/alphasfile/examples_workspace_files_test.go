package alphasfile

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for examples/workspace_files: the top-level workspace
// block must resolve to the four declared files with their content already
// substituted, and the service must land on the templated branch. Pure
// resolution — nothing is written, cloned or spawned.
func TestExampleWorkspaceFilesResolves(t *testing.T) {
	root, err := filepath.Abs("../../examples/workspace_files")
	if err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(root, "workspaces", "feature")
	spec, err := RenderWorkspace(filepath.Join(root, "Alphasfile"), &invocation.InvocationState{
		Dir: wsDir, Workspace: "feature", StateDir: wsDir,
		FsHash: "abcd1234ef567890", TmpDir: "/tmp/zordon-abcd1234ef567890",
	})
	if err != nil {
		t.Fatalf("RenderWorkspace: %v", err)
	}

	byName := map[string]*WorkspaceFile{}
	for _, f := range spec.Files {
		byName[f.Name] = f
	}
	for _, want := range []string{"claude", "devcontainer", "settings", "ignore"} {
		if byName[want] == nil {
			t.Fatalf("no file %q resolved; got %d files", want, len(spec.Files))
		}
	}

	// create + source: the template must have been rendered, not copied.
	claude := byName["claude"]
	if claude.Op != OpCreate {
		t.Errorf("claude op = %q, want %q", claude.Op, OpCreate)
	}
	if !strings.Contains(claude.Body, "Workspace feature") {
		t.Errorf("claude body did not interpolate workspace.name:\n%s", claude.Body)
	}
	if strings.Contains(claude.Body, "${") {
		t.Errorf("claude body still holds an unrendered interpolation:\n%s", claude.Body)
	}

	// The devcontainer carries a concrete port, which is the only reason a
	// static file can name a server started later.
	dc := byName["devcontainer"]
	if !strings.Contains(dc.Body, `"name": "zordon-feature"`) {
		t.Errorf("devcontainer body = %s", dc.Body)
	}
	port := WorkspacePort("abcd1234ef567890")
	if want := "--listen 127.0.0.1:" + strconv.Itoa(port); !strings.Contains(dc.Body, want) {
		t.Errorf("devcontainer body = %s, want it to contain %q", dc.Body, want)
	}

	if got := byName["settings"].Op; got != OpMerge {
		t.Errorf("settings op = %q, want %q", got, OpMerge)
	}
	if got := byName["ignore"].Op; got != OpRegion {
		t.Errorf("ignore op = %q, want %q", got, OpRegion)
	}

	br, err := spec.BranchFor("app", "go")
	if err != nil {
		t.Fatalf("BranchFor: %v", err)
	}
	if want := "zordon/feature/app"; br != want {
		t.Errorf("BranchFor = %q, want %q", br, want)
	}
}
