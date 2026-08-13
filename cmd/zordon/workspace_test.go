package main

import (
	"path/filepath"
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
	if _, err := applyWorkspaceTo(discardLogger(), discardWriter{}, root, ws); err != nil {
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
