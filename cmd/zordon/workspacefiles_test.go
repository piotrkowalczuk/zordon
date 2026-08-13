package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zdoc"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestWorkspaceFilePath_accepts(t *testing.T) {
	inv := namedWorkspaceInv("/proj", "feature")

	cases := map[string]string{
		"top level":                    "CLAUDE.md",
		"nested dotdir":                ".claude/settings.json",
		"devcontainer":                 ".devcontainer/devcontainer.json",
		"deeply nested":                "a/b/c/d.txt",
		"interior climb":               "a/../CLAUDE.md",
		"name resembling src":          "srcnotes.md",
		"name resembling etc":          "etcetera.txt",
		"src as a filename, not a dir": "src.md",
	}

	for hint, rel := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := workspaceFilePath(inv, nil, rel)
			if err != nil {
				t.Fatalf("workspaceFilePath(%q) = %v, want it accepted", rel, err)
			}
			if !zfs.Within(inv.Dir, got) {
				t.Errorf("workspaceFilePath(%q) = %q, outside %q", rel, got, inv.Dir)
			}
		})
	}
}

// TestWorkspaceFilePath_rejects covers the two reasons a target is refused:
// it leaves the workspace, or it lands in a subtree owned by zordon or by a
// service's git checkout.
func TestWorkspaceFilePath_rejects(t *testing.T) {
	inv := namedWorkspaceInv("/proj", "feature")

	cases := map[string]struct {
		rel  string
		want string
	}{
		"parent climb":     {rel: "../escape.md", want: "escapes the workspace"},
		"deep climb":       {rel: "../../../etc/passwd", want: "escapes the workspace"},
		"absolute":         {rel: "/etc/passwd", want: "must be relative"},
		"service checkout": {rel: "src/api/CLAUDE.md", want: "src/"},
		"src itself":       {rel: "src/x", want: "src/"},
		"build output":     {rel: "bin/tool", want: "bin/"},
		"service config":   {rel: "etc/api/conf", want: "etc/"},
		"service state":    {rel: "var/api/data", want: "var/"},
		"workspace marker": {rel: invocation.WorkspaceMarker, want: "zordon's own"},
		"start lock":       {rel: "start.lock", want: "zordon's own"},
		"alpha log":        {rel: "alpha.log", want: "zordon's own"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := workspaceFilePath(inv, nil, c.rel)
			if err == nil {
				t.Fatalf("workspaceFilePath(%q) succeeded, want it refused", c.rel)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestWorkspaceFilePath_mainRejectsWorkspacesTree pins the case unique to
// main: its workspace dir IS the project root, so every other workspace's
// directory sits underneath it — and none of them are main's to write into.
func TestWorkspaceFilePath_mainRejectsWorkspacesTree(t *testing.T) {
	inv := mainWorkspaceInv("/proj")

	if _, err := workspaceFilePath(inv, nil, "CLAUDE.md"); err != nil {
		t.Fatalf("main should accept a plain project file: %v", err)
	}
	for _, rel := range []string{
		"workspaces/feature/CLAUDE.md",
		"workspaces/main/src/api/x",
		"workspaces/other",
	} {
		_, err := workspaceFilePath(inv, nil, rel)
		if err == nil {
			t.Errorf("workspaceFilePath(%q) succeeded in main, want it refused", rel)
		}
	}
}

// TestWorkspaceFilePath_mainRejectsServiceSources is the hole the named-
// workspace tests could not see. In a named workspace every checkout lives
// under <StateDir>/src, so a guard keyed on StateDir covers them. In main
// there are no checkouts — services build IN PLACE from the developer's own
// tree — so that guard matches nothing and a declared path walks straight into
// a service's working tree.
func TestWorkspaceFilePath_mainRejectsServiceSources(t *testing.T) {
	inv := mainWorkspaceInv("/proj")
	protected := []string{
		filepath.FromSlash("/proj/src/app"),
		filepath.FromSlash("/proj/services/api"),
	}

	for hint, rel := range map[string]string{
		"inside a service source":       "src/app/LEAKED.md",
		"the service source dir itself": "src/app",
		"deep inside a service source":  "src/app/internal/x/y.go",
		"a second service":              "services/api/CLAUDE.md",
	} {
		t.Run(hint, func(t *testing.T) {
			_, err := workspaceFilePath(inv, protected, rel)
			if err == nil {
				t.Fatalf("workspaceFilePath(%q) succeeded, want it refused", rel)
			}
			if !strings.Contains(err.Error(), "source") {
				t.Errorf("error = %v, want it to say the path is a service's source", err)
			}
		})
	}

	// Neighbours that merely share a prefix are none of the guard's business.
	for hint, rel := range map[string]string{
		"sibling of a service dir":             "src/other/x",
		"prefix impostor":                      "src/appendix/x",
		"unrelated top level":                  "docs/README.md",
		"service rooted at the project itself": "CLAUDE.md",
	} {
		t.Run(hint, func(t *testing.T) {
			if _, err := workspaceFilePath(inv, protected, rel); err != nil {
				t.Errorf("workspaceFilePath(%q) = %v, want it accepted", rel, err)
			}
		})
	}
}

// TestWorkspaceFilePath_sourceTreeContainingTheWorkspace is the other side of
// the guard. A monorepo service declares src{path="../.."} — the repo root —
// and a named workspace lives at <root>/workspaces/<name>, INSIDE it. Treating
// that as "writing into a service's source" would refuse every workspace file
// there is; the checkouts the workspace actually owns are guarded separately.
func TestWorkspaceFilePath_sourceTreeContainingTheWorkspace(t *testing.T) {
	inv := namedWorkspaceInv("/proj", "feature")
	protected := []string{filepath.FromSlash("/proj")}

	for _, rel := range []string{"CLAUDE.md", ".claude/settings.json", ".devcontainer/devcontainer.json"} {
		if _, err := workspaceFilePath(inv, protected, rel); err != nil {
			t.Errorf("workspaceFilePath(%q) = %v, want it accepted", rel, err)
		}
	}
	// The workspace's own checkout stays refused, by the state-dir rule.
	if _, err := workspaceFilePath(inv, protected, "src/app/x"); err == nil {
		t.Error("a path into the workspace's own checkout was accepted")
	}
}

func TestApplyWorkspaceFiles_create(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)

	spec := specWith(&alphasfile.WorkspaceFile{
		Name: "claude", Path: "CLAUDE.md", Op: alphasfile.OpCreate, Body: "# hello\n",
	})
	apply(t, spec, inv)

	if got := readBack(t, filepath.Join(inv.Dir, "CLAUDE.md")); got != "# hello\n" {
		t.Errorf("CLAUDE.md = %q", got)
	}
}

func TestApplyWorkspaceFiles_createsParentDirs(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)

	spec := specWith(&alphasfile.WorkspaceFile{
		Name: "dc", Path: ".devcontainer/devcontainer.json", Op: alphasfile.OpCreate, Body: "{}\n",
	})
	apply(t, spec, inv)

	if got := readBack(t, filepath.Join(inv.Dir, ".devcontainer", "devcontainer.json")); got != "{}\n" {
		t.Errorf("devcontainer.json = %q", got)
	}
}

func TestApplyWorkspaceFiles_mergeKeepsSiblings(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)
	target := filepath.Join(inv.Dir, ".claude", "settings.json")
	mustWrite(t, target, `{"model":"opus"}`)

	spec := specWith(&alphasfile.WorkspaceFile{
		Name: "settings", Path: ".claude/settings.json", Op: alphasfile.OpMerge,
		Format: zdoc.FormatJSON,
		Data:   map[string]any{"hooks": map[string]any{"PostToolUse": []any{"make fmt"}}},
	})
	apply(t, spec, inv)

	got := readBack(t, target)
	for _, want := range []string{`"model": "opus"`, `"hooks"`, `"make fmt"`} {
		if !strings.Contains(got, want) {
			t.Errorf("settings.json = %s, want it to contain %q", got, want)
		}
	}
}

func TestApplyWorkspaceFiles_regionAppends(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)
	target := filepath.Join(inv.Dir, ".gitignore")
	mustWrite(t, target, "node_modules/\n")

	spec := specWith(&alphasfile.WorkspaceFile{
		Name: "ignore", Path: ".gitignore", Op: alphasfile.OpRegion, Body: "build/", Comment: "#",
	})
	apply(t, spec, inv)

	got := readBack(t, target)
	if !strings.HasPrefix(got, "node_modules/\n") {
		t.Errorf(".gitignore = %q, want the original first line kept", got)
	}
	if !strings.Contains(got, "build/") {
		t.Errorf(".gitignore = %q, want the region body", got)
	}
}

// TestApplyWorkspaceFiles_idempotent is the contract behind running apply as
// often as you like: a second pass must leave every byte alone.
func TestApplyWorkspaceFiles_idempotent(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)
	mustWrite(t, filepath.Join(inv.Dir, ".gitignore"), "node_modules/\n")

	spec := specWith(
		&alphasfile.WorkspaceFile{Name: "claude", Path: "CLAUDE.md", Op: alphasfile.OpCreate, Body: "# hi\n"},
		&alphasfile.WorkspaceFile{
			Name: "settings", Path: ".claude/settings.json", Op: alphasfile.OpMerge,
			Format: zdoc.FormatJSON, Data: map[string]any{"hooks": map[string]any{"a": []any{"b"}}},
		},
		&alphasfile.WorkspaceFile{Name: "ignore", Path: ".gitignore", Op: alphasfile.OpRegion, Body: "build/"},
	)

	apply(t, spec, inv)
	first := map[string]string{}
	for _, rel := range []string{"CLAUDE.md", ".claude/settings.json", ".gitignore"} {
		first[rel] = readBack(t, filepath.Join(inv.Dir, filepath.FromSlash(rel)))
	}

	var out strings.Builder
	if err := applyWorkspaceFiles(spec, inv, nil, zlog.New(io.Discard, false), &out); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for rel, want := range first {
		if got := readBack(t, filepath.Join(inv.Dir, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s changed on the second apply:\nfirst:  %q\nsecond: %q", rel, want, got)
		}
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("second apply said %q, want it to report no change", out.String())
	}
}

// TestApplyWorkspaceFiles_preservesModeOfForeignFiles is the wiring half of
// zfs.AtomicWriteKeepingMode: that function can be correct and still unused.
// merge and region edit files somebody else created, so demoting a 0644 config
// to 0600 breaks whatever else on the machine reads it. create owns its files
// outright and keeps zordon's private mode.
func TestApplyWorkspaceFiles_preservesModeOfForeignFiles(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)

	settings := filepath.Join(inv.Dir, ".claude", "settings.json")
	ignore := filepath.Join(inv.Dir, ".gitignore")
	mustWrite(t, settings, `{"model":"opus"}`)
	mustWrite(t, ignore, "node_modules/\n")
	mustChmod(t, settings, 0o644)
	mustChmod(t, ignore, 0o664)

	apply(t, specWith(
		&alphasfile.WorkspaceFile{
			Name: "settings", Path: ".claude/settings.json", Op: alphasfile.OpMerge,
			Format: zdoc.FormatJSON, Data: map[string]any{"env": map[string]any{"A": "b"}},
		},
		&alphasfile.WorkspaceFile{Name: "ignore", Path: ".gitignore", Op: alphasfile.OpRegion, Body: "build/"},
	), inv)

	for path, want := range map[string]os.FileMode{settings: 0o644, ignore: 0o664} {
		fi, err := zfs.Stat(path)
		if err != nil {
			t.Fatalf("Stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want the mode the user's file already had (%v)", filepath.Base(path), got, want)
		}
	}
}

// TestApplyWorkspaceFiles_rejectsDuplicateTargets: two blocks aimed at one
// file would let declaration order decide the result, and a merge racing a
// create on the same path is never what anybody meant. Distinct from the
// "declared twice" check in alphasfile, which catches a repeated block NAME —
// these blocks have different names and the same target.
func TestApplyWorkspaceFiles_rejectsDuplicateTargets(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)

	spec := specWith(
		&alphasfile.WorkspaceFile{Name: "first", Path: "CLAUDE.md", Op: alphasfile.OpCreate, Body: "a\n"},
		&alphasfile.WorkspaceFile{Name: "second", Path: "./CLAUDE.md", Op: alphasfile.OpCreate, Body: "b\n"},
	)

	var out strings.Builder
	err := applyWorkspaceFiles(spec, inv, nil, discardLogger(), &out)
	if err == nil {
		t.Fatal("applyWorkspaceFiles succeeded, want the duplicate target refused")
	}
	for _, want := range []string{"first", "second", "CLAUDE.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
	if zfs.Exists(filepath.Join(inv.Dir, "CLAUDE.md")) {
		t.Error("a file was written despite the conflict")
	}
}

// TestApplyWorkspaceFiles_validatesBeforeWriting keeps a manifest with one bad
// path from leaving a half-written workspace behind.
func TestApplyWorkspaceFiles_validatesBeforeWriting(t *testing.T) {
	root := t.TempDir()
	inv := namedWorkspaceInv(root, "feature")
	mustDir(t, inv.Dir)

	spec := specWith(
		&alphasfile.WorkspaceFile{Name: "good", Path: "CLAUDE.md", Op: alphasfile.OpCreate, Body: "# hi\n"},
		&alphasfile.WorkspaceFile{Name: "bad", Path: "src/api/x", Op: alphasfile.OpCreate, Body: "nope\n"},
	)

	var out strings.Builder
	err := applyWorkspaceFiles(spec, inv, nil, zlog.New(io.Discard, false), &out)
	if err == nil {
		t.Fatal("applyWorkspaceFiles succeeded, want the bad path refused")
	}
	if !strings.Contains(err.Error(), `"bad"`) {
		t.Errorf("error = %v, want it to name the offending file", err)
	}
	if zfs.Exists(filepath.Join(inv.Dir, "CLAUDE.md")) {
		t.Error("the valid file was written even though another path was refused")
	}
}

func discardLogger() *zlog.Logger { return zlog.New(io.Discard, false) }

func namedWorkspaceInv(root, name string) *invocation.InvocationState {
	dir := filepath.Join(root, "workspaces", name)
	return &invocation.InvocationState{
		Dir: dir, Workspace: name, StateDir: dir,
		FsHash: "abcd1234ef567890", TmpDir: filepath.Join(root, "tmp"),
	}
}

func mainWorkspaceInv(root string) *invocation.InvocationState {
	return &invocation.InvocationState{
		Dir: root, Workspace: invocation.MainWorkspace,
		StateDir: filepath.Join(root, "workspaces", invocation.MainWorkspace),
		FsHash:   "abcd1234ef567890", TmpDir: filepath.Join(root, "tmp"),
	}
}

func specWith(files ...*alphasfile.WorkspaceFile) *alphasfile.WorkspaceSpec {
	return &alphasfile.WorkspaceSpec{Files: files}
}

func apply(t *testing.T, spec *alphasfile.WorkspaceSpec, inv *invocation.InvocationState) {
	t.Helper()
	var out strings.Builder
	if err := applyWorkspaceFiles(spec, inv, nil, zlog.New(io.Discard, false), &out); err != nil {
		t.Fatalf("applyWorkspaceFiles: %v", err)
	}
}

func mustDir(t *testing.T, dir string) {
	t.Helper()
	if err := zfs.EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir %s: %v", dir, err)
	}
}

// mustChmod reaches for os directly: this is a test ABOUT file modes, and zfs
// deliberately exposes no chmod — it owns one private mode by design.
func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	mustDir(t, filepath.Dir(path))
	if err := zfs.AtomicWrite(path, []byte(body)); err != nil {
		t.Fatalf("AtomicWrite %s: %v", path, err)
	}
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	b, err := zfs.Read(path)
	if err != nil {
		t.Fatalf("Read %s: %v", path, err)
	}
	return string(b)
}
