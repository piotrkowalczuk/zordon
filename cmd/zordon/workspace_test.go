package main

import (
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// TestApplyTarget is the guard on which directory `zordon workspace apply`
// writes into.
//
// Defaulting to main is actively dangerous rather than merely surprising: the
// documented way to work in a workspace is to cd into it (SKILL.md says so),
// and main's directory is the project root — the developer's real repository,
// with its tracked CLAUDE.md and .claude/settings.json. A plain `apply` run
// from inside a workspace must act on THAT workspace.
func TestApplyTarget(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, invocation.AlphasfileName), "sysenv = []\n")

	wsDir := filepath.Join(root, "workspaces", "feature")
	mustDir(t, wsDir)
	mustWrite(t, filepath.Join(wsDir, invocation.WorkspaceMarker), "")

	nested := filepath.Join(wsDir, "src", "app")
	mustDir(t, nested)

	cases := map[string]struct {
		flag invocation.WorkspaceName
		cwd  string
		want string
	}{
		"project root, no flag":       {cwd: root, want: invocation.MainWorkspace},
		"inside a workspace, no flag": {cwd: wsDir, want: "feature"},
		"deep inside a workspace":     {cwd: nested, want: "feature"},
		"flag wins over cwd":          {flag: "feature", cwd: root, want: "feature"},
		"flag can name main":          {flag: invocation.MainWorkspace, cwd: wsDir, want: invocation.MainWorkspace},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := applyTarget(c.flag, c.cwd)
			if err != nil {
				t.Fatalf("applyTarget: %v", err)
			}
			if got.Name() != c.want {
				t.Errorf("applyTarget(%q, %q) = %q, want %q", c.flag, c.cwd, got.Name(), c.want)
			}
		})
	}
}

// TestApplyWorkspaceTo_passesServiceSources closes the gap between a guard
// that works and a guard that is wired up. workspaceFilePath is tested
// directly, but nothing else proves the caller hands it the services' source
// directories — pass nil there and the guard silently protects nothing, which
// is exactly how the original hole looked.
func TestApplyWorkspaceTo_passesServiceSources(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, invocation.AlphasfileName), `
workspace {
  file "leak" {
    path = "src/app/LEAKED.md"
    create { body = "should never be written\n" }
  }
}

service "go" "app" {
  src {
    path = "./src/app"
  }
  runtime { cmd = ["./app"] }
}
`)
	mustDir(t, filepath.Join(root, "src", "app"))

	_, err := applyWorkspaceTo(discardLogger(), io.Discard, root, invocation.MainWorkspace)
	if err == nil {
		t.Fatal("applyWorkspaceTo succeeded, want the write into a service source refused")
	}
	if !strings.Contains(err.Error(), "source tree") {
		t.Errorf("error = %v, want it to name the service's source tree", err)
	}
	if zfs.Exists(filepath.Join(root, "src", "app", "LEAKED.md")) {
		t.Error("the file was written into the service's source tree")
	}
}

// TestRenderWorkspaceFor_writesNothing is the fast-feedback guard on what
// `workspace service add` uses. Adding a checkout is no reason to rewrite the
// workspace's files, and an agent that has edited CLAUDE.md must not lose that
// edit to an unrelated command.
//
// The example asserts the same thing end to end, but only `make e2e` runs it;
// this fails in `go test ./...` instead, which is where a regression gets
// noticed.
func TestRenderWorkspaceFor_writesNothing(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, invocation.AlphasfileName), `
workspace {
  file "claude" {
    path = "CLAUDE.md"
    create { body = "generated for ${workspace.name}\n" }
  }
  file "settings" {
    path = ".claude/settings.json"
    merge { data = { env = { A = "b" } } }
  }
}
`)
	wsDir := filepath.Join(root, "workspaces", "feature")
	mustDir(t, wsDir)
	mustWrite(t, filepath.Join(wsDir, invocation.WorkspaceMarker), "")

	edited := filepath.Join(wsDir, "CLAUDE.md")
	mustWrite(t, edited, "EDITED BY THE USER\n")

	spec, err := renderWorkspaceFor(root, "feature")
	if err != nil {
		t.Fatalf("renderWorkspaceFor: %v", err)
	}
	if len(spec.Files) != 2 {
		t.Fatalf("got %d files, want the block resolved", len(spec.Files))
	}

	if got := readBack(t, edited); got != "EDITED BY THE USER\n" {
		t.Errorf("the edited file was rewritten: %q", got)
	}
	if zfs.Exists(filepath.Join(wsDir, ".claude", "settings.json")) {
		t.Error("a file that did not exist was created")
	}
}

// TestRunWorkspaceServiceAdd_leavesGeneratedFilesAlone covers the WIRING that
// TestRenderWorkspaceFor_writesNothing cannot: swapping the call back to
// applyWorkspaceHere would leave that test green while the command started
// overwriting files again. It does real git work, which is why it is the one
// heavier test here — the alternative was leaving this to `make e2e`.
func TestRunWorkspaceServiceAdd_leavesGeneratedFilesAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, invocation.AlphasfileName), `
workspace {
  file "claude" {
    path = "CLAUDE.md"
    create { body = "generated for ${workspace.name}\n" }
  }
}

service "go" "app" {
  src { path = "." }
  runtime { cmd = ["./app"] }
}
`)
	mustWrite(t, filepath.Join(root, "main.go"), "package main\n")
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "-c", "user.email=a@b", "-c", "user.name=z", "commit", "-q", "-m", "init")

	wsDir := filepath.Join(root, "workspaces", "feature")
	mustDir(t, wsDir)
	mustWrite(t, filepath.Join(wsDir, invocation.WorkspaceMarker), "")

	edited := filepath.Join(wsDir, "CLAUDE.md")
	mustWrite(t, edited, "EDITED BY THE USER\n")

	t.Chdir(root)
	err := runWorkspaceServiceAdd(t.Context(), discardLogger(), io.Discard,
		invocation.WorkspaceName("feature"), []string{"app"}, t.TempDir())
	if err != nil {
		t.Fatalf("runWorkspaceServiceAdd: %v", err)
	}

	if got := readBack(t, edited); got != "EDITED BY THE USER\n" {
		t.Errorf("service add rewrote the edited file: %q", got)
	}
	if !zfs.Exists(filepath.Join(wsDir, "src", "app", ".git")) {
		t.Error("the service was not actually checked out, so the assertion above proves nothing")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestServiceSourceDirs pins what feeds that guard.
func TestServiceSourceDirs(t *testing.T) {
	root := t.TempDir()
	af := filepath.Join(root, invocation.AlphasfileName)
	mustWrite(t, af, `
service "go" "app" {
  src { path = "./src/app" }
  runtime { cmd = ["./app"] }
}

service "go" "tool" {
  package = "example.com/tool@v1"
  runtime { cmd = ["./tool"] }
}
`)

	got, err := serviceSourceDirs(af)
	if err != nil {
		t.Fatalf("serviceSourceDirs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only the service with a local source", got)
	}
	if want := filepath.Join(root, "src", "app"); got[0] != want {
		t.Errorf("got %q, want the resolved absolute source %q", got[0], want)
	}
}

// TestApplyTarget_writesIntoTheWorkspaceItIsIn is the end-to-end shape of the
// same bug: a file declared for the workspace must land in the workspace, not
// in the project root next to it.
func TestApplyTarget_writesIntoTheWorkspaceItIsIn(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, invocation.AlphasfileName), `
workspace {
  file "marker" {
    path = "WHERE.md"
    create { body = "workspace: ${workspace.name}\n" }
  }
}
`)
	wsDir := filepath.Join(root, "workspaces", "feature")
	mustDir(t, wsDir)
	mustWrite(t, filepath.Join(wsDir, invocation.WorkspaceMarker), "")

	// A hand-written file in the project root that must survive untouched.
	rootFile := filepath.Join(root, "WHERE.md")
	mustWrite(t, rootFile, "HAND WRITTEN\n")

	ws, err := applyTarget("", wsDir)
	if err != nil {
		t.Fatalf("applyTarget: %v", err)
	}
	if _, err := applyWorkspaceTo(discardLogger(), io.Discard, root, ws); err != nil {
		t.Fatalf("applyWorkspaceTo: %v", err)
	}

	if got := readBack(t, rootFile); got != "HAND WRITTEN\n" {
		t.Errorf("the project root's file was rewritten: %q", got)
	}
	wsFile := filepath.Join(wsDir, "WHERE.md")
	if !zfs.Exists(wsFile) {
		t.Fatalf("nothing was written into the workspace at %s", wsFile)
	}
	if got := readBack(t, wsFile); got != "workspace: feature\n" {
		t.Errorf("workspace file = %q, want it rendered for \"feature\"", got)
	}
}
