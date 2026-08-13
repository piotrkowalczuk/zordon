package alphasfile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zdoc"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestRenderWorkspace_noBlock(t *testing.T) {
	spec := mustRenderWorkspace(t, `service "go" "api" {
  git { url = "github.com/acme/api" }
  runtime { cmd = ["./api"] }
}`, nil)
	if len(spec.Files) != 0 {
		t.Errorf("got %d files, want none", len(spec.Files))
	}
	br, err := spec.BranchFor("api", "go")
	if err != nil {
		t.Fatalf("BranchFor: %v", err)
	}
	if want := "zordon/feature/api"; br != want {
		t.Errorf("BranchFor = %q, want %q", br, want)
	}
}

func TestRenderWorkspace_create(t *testing.T) {
	spec := mustRenderWorkspace(t, `
workspace {
  file "claude" {
    path = "CLAUDE.md"
    create { body = "workspace ${workspace.name} at ${workspace.dir}\n" }
  }
}
`, nil)
	f := onlyFile(t, spec)
	if f.Op != OpCreate {
		t.Errorf("Op = %q, want %q", f.Op, OpCreate)
	}
	if f.Path != "CLAUDE.md" {
		t.Errorf("Path = %q", f.Path)
	}
	if !strings.HasPrefix(f.Body, "workspace feature at ") {
		t.Errorf("Body = %q, want the workspace name and dir interpolated", f.Body)
	}
}

func TestRenderWorkspace_createFromSource(t *testing.T) {
	spec := mustRenderWorkspace(t, `
workspace {
  file "claude" {
    path = "CLAUDE.md"
    create { source = "templates/CLAUDE.md" }
  }
}
`, map[string]string{"templates/CLAUDE.md": "# from a template\n"})
	if got := onlyFile(t, spec).Body; got != "# from a template\n" {
		t.Errorf("Body = %q", got)
	}
}

func TestRenderWorkspace_merge(t *testing.T) {
	spec := mustRenderWorkspace(t, `
workspace {
  file "settings" {
    path = ".claude/settings.json"
    merge { data = { hooks = { PostToolUse = ["make fmt"] } } }
  }
}
`, nil)
	f := onlyFile(t, spec)
	if f.Op != OpMerge {
		t.Errorf("Op = %q, want %q", f.Op, OpMerge)
	}
	if f.Format != zdoc.FormatJSON {
		t.Errorf("Format = %q, want json inferred from the extension", f.Format)
	}
	m, ok := f.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data is %T, want an object", f.Data)
	}
	if _, ok := m["hooks"]; !ok {
		t.Errorf("Data = %v, want a hooks key", m)
	}
}

func TestRenderWorkspace_region(t *testing.T) {
	spec := mustRenderWorkspace(t, `
workspace {
  file "ignore" {
    path = ".gitignore"
    region { body = "workspaces/" }
  }
}
`, nil)
	f := onlyFile(t, spec)
	if f.Op != OpRegion {
		t.Errorf("Op = %q, want %q", f.Op, OpRegion)
	}
	if f.Body != "workspaces/" {
		t.Errorf("Body = %q", f.Body)
	}
}

func TestRenderWorkspace_invalid(t *testing.T) {
	cases := map[string]struct {
		block string
		want  string
	}{
		"no operation": {
			block: `
file "f" {
  path = "x"
}`,
			want: "exactly one of create/merge/region",
		},
		"two operations": {
			block: `
file "f" {
  path = "x"
  create { body = "a" }
  region { body = "b" }
}`,
			want: "exactly one of create/merge/region",
		},
		"create with both body and source": {
			block: `
file "f" {
  path = "x"
  create {
    body   = "a"
    source = "b"
  }
}`,
			want: "set body or source, not both",
		},
		"create with neither": {
			block: `
file "f" {
  path = "x"
  create {}
}`,
			want: "needs body or source",
		},
		"empty path": {
			block: `
file "f" {
  path = ""
  create { body = "a" }
}`,
			want: "path is empty",
		},
		"duplicate name": {
			block: `
file "f" {
  path = "a"
  create { body = "x" }
}
file "f" {
  path = "b"
  create { body = "y" }
}`,
			want: "declared twice",
		},
		"merge on an unknown extension": {
			block: `
file "f" {
  path = "notes.txt"
  merge { data = { a = 1 } }
}`,
			want: "cannot tell the format",
		},
		"merge with a bogus format": {
			block: `
file "f" {
  path = "x.json"
  merge {
    data   = { a = 1 }
    format = "xml"
  }
}`,
			want: "unknown format",
		},
		"merge data is not an object": {
			block: `
file "f" {
  path = "x.json"
  merge { data = [1, 2] }
}`,
			want: "must be an object",
		},
		"region on json": {
			block: `
file "f" {
  path = "x.json"
  region { body = "a" }
}`,
			want: "region cannot be used on a JSON file",
		},
		"source escaping the project root": {
			block: `
file "f" {
  path = "x"
  create { source = "../../../etc/passwd" }
}`,
			want: "inside the project root",
		},
		"absolute source": {
			block: `
file "f" {
  path = "x"
  create { source = "/etc/passwd" }
}`,
			want: "inside the project root",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			_, err := renderWorkspace(t, "workspace {\n"+c.block+"\n}\n", nil)
			if err == nil {
				t.Fatalf("RenderWorkspace succeeded, want an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestRenderWorkspace_runtimeRefsRejected is the guard that keeps a generated
// file honest: everything here resolves only once alpha runs, long after
// these files are written, so silently resolving it would bake in a value
// that never matches reality.
func TestRenderWorkspace_runtimeRefsRejected(t *testing.T) {
	cases := map[string]struct {
		expr string
		want string
	}{
		"picked port":      {expr: `"${net::pickport()}"`, want: "not available in the top-level workspace block"},
		"service ref":      {expr: `"${service.go.api.vars.port}"`, want: "do not exist yet when workspace files are written"},
		"self ref":         {expr: `"${self.dir}"`, want: "has no meaning in the top-level workspace block"},
		"service checkout": {expr: `"${fs::src()}"`, want: "not available in the top-level workspace block"},
		"source hash":      {expr: `"${src::hash()}"`, want: "not available in the top-level workspace block"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			src := `
service "go" "api" {
  git { url = "github.com/acme/api" }
  runtime { cmd = ["./api"] }
}
workspace {
  file "f" {
    path = "x"
    create { body = ` + c.expr + ` }
  }
}
`
			_, err := renderWorkspace(t, src, nil)
			if err == nil {
				t.Fatalf("RenderWorkspace succeeded for %s, want an error", c.expr)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestRenderWorkspace_workspaceVars(t *testing.T) {
	spec := mustRenderWorkspace(t, `
workspace {
  file "f" {
    path = "x.json"
    create { body = enc::json({ name = workspace.name, hash = workspace.hash, port = workspace.port }) }
  }
}
`, nil)
	body := onlyFile(t, spec).Body
	for _, want := range []string{`"name": "feature"`, `"hash": "abcd1234ef567890"`, `"port": `} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want it to contain %q", body, want)
		}
	}
	if strings.Contains(body, `"port": 0`) {
		t.Errorf("body = %s, want a derived port", body)
	}
}

func TestWorkspacePort_stable(t *testing.T) {
	const hash = "abcd1234ef567890"
	first := WorkspacePort(hash)
	for range 10 {
		if got := WorkspacePort(hash); got != first {
			t.Fatalf("WorkspacePort(%q) = %d then %d, want a stable value", hash, first, got)
		}
	}
	if first < 20000 || first >= 40000 {
		t.Errorf("WorkspacePort = %d, want it inside the documented range", first)
	}
	if other := WorkspacePort("0000000000000000"); other == first {
		t.Errorf("two workspaces share port %d", first)
	}
}

func TestWorkspaceSpec_BranchFor(t *testing.T) {
	cases := map[string]struct {
		branch string
		want   string
	}{
		"default":            {branch: "", want: "zordon/feature/api"},
		"custom prefix":      {branch: `branch = "wip/${workspace.name}/${service.name}"`, want: "wip/feature/api"},
		"toolchain included": {branch: `branch = "z/${service.toolchain}/${service.name}"`, want: "z/go/api"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			spec := mustRenderWorkspace(t, "workspace {\n  "+c.branch+"\n}\n", nil)
			got, err := spec.BranchFor("api", "go")
			if err != nil {
				t.Fatalf("BranchFor: %v", err)
			}
			if got != c.want {
				t.Errorf("BranchFor = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDefaultBranchFor_matchesTemplate pins the two spellings of the default
// together: DefaultBranchTemplate is what the manifest and the docs describe,
// DefaultBranchFor is the fallback alpha and status use for a service resolved
// before WorkspaceBranch existed. If they ever disagree, a checkout and the
// branch it is expected on would silently diverge.
func TestDefaultBranchFor_matchesTemplate(t *testing.T) {
	spec := mustRenderWorkspace(t, "workspace {\n}\n", nil)
	rendered, err := spec.BranchFor("api", "go")
	if err != nil {
		t.Fatalf("BranchFor: %v", err)
	}
	if want := DefaultBranchFor("feature", "api"); rendered != want {
		t.Errorf("DefaultBranchTemplate renders %q but DefaultBranchFor gives %q", rendered, want)
	}
}

func TestWorkspaceSpec_BranchesFor_collision(t *testing.T) {
	spec := mustRenderWorkspace(t, `workspace { branch = "zordon/${workspace.name}" }`, nil)
	_, err := spec.BranchesFor([]*ServiceMeta{
		{Name: "api", Toolchain: "go"},
		{Name: "web", Toolchain: "go"},
	})
	if err == nil {
		t.Fatal("BranchesFor succeeded, want a collision error")
	}
	for _, want := range []string{"more than one service", "api, web", "${service.name}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestWorkspaceSpec_BranchFor_invalidName(t *testing.T) {
	cases := map[string]string{
		"space":          `branch = "my branch/${service.name}"`,
		"double dot":     `branch = "a..b/${service.name}"`,
		"leading slash":  `branch = "/${service.name}"`,
		"trailing slash": `branch = "${service.name}/"`,
		"dot segment":    `branch = "a/.${service.name}"`,
		"lock suffix":    `branch = "a/${service.name}.lock"`,
		"empty":          `branch = ""`,
	}

	for hint, branch := range cases {
		t.Run(hint, func(t *testing.T) {
			spec := mustRenderWorkspace(t, "workspace {\n  "+branch+"\n}\n", nil)
			if _, err := spec.BranchFor("api", "go"); err == nil {
				t.Fatalf("BranchFor accepted %s, want a git-ref validation error", branch)
			}
		})
	}
}

func mustRenderWorkspace(t *testing.T, src string, extra map[string]string) *WorkspaceSpec {
	t.Helper()
	spec, err := renderWorkspace(t, src, extra)
	if err != nil {
		t.Fatalf("RenderWorkspace: %v", err)
	}
	return spec
}

// renderWorkspace lays out a project root with an Alphasfile (plus any extra
// files a `source` points at) and resolves it as workspace "feature".
func renderWorkspace(t *testing.T, src string, extra map[string]string) (*WorkspaceSpec, error) {
	t.Helper()
	root := t.TempDir()
	af := filepath.Join(root, "Alphasfile")
	writeFile(t, af, src)
	for rel, body := range extra {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), body)
	}
	return RenderWorkspace(af, &invocation.InvocationState{
		Dir:       filepath.Join(root, "workspaces", "feature"),
		Workspace: "feature",
		StateDir:  filepath.Join(root, "workspaces", "feature"),
		FsHash:    "abcd1234ef567890",
		TmpDir:    "/tmp/zordon-abcd1234ef567890",
	})
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := zfs.EnsureDir(filepath.Dir(path)); err != nil {
		t.Fatalf("EnsureDir %s: %v", filepath.Dir(path), err)
	}
	if err := zfs.AtomicWrite(path, []byte(body)); err != nil {
		t.Fatalf("AtomicWrite %s: %v", path, err)
	}
}

func onlyFile(t *testing.T, spec *WorkspaceSpec) *WorkspaceFile {
	t.Helper()
	if len(spec.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(spec.Files))
	}
	return spec.Files[0]
}
