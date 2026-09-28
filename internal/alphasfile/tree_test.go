package alphasfile

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestLoadTree_resolvesImportRelativeToImporter(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":           `import "./a/Alphasfile.a" { components = ["a"] }`,
		"a/Alphasfile.a":       "component \"a\" {\n  import \"../b/Alphasfile.b\" { components = [\"b\"] }\n}\n",
		"b/Alphasfile.b":       `component "b" {}`,
		"b/Alphasfile.ignored": `component "x" {}`,
	})
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{root, filepath.Join(dir, "a/Alphasfile.a"), filepath.Join(dir, "b/Alphasfile.b")}
	if got := tree.Files(); !equalStrs(got, want) {
		t.Errorf("Files = %v, want %v", got, want)
	}
}

func TestLoadTree_loadsOnce_diamond(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   "import \"./Alphasfile.a\" { components = [\"a\"] }\nimport \"./Alphasfile.b\" { components = [\"b\"] }\n",
		"Alphasfile.a": "component \"a\" {\n  import \"./Alphasfile.c\" { components = [\"c\"] }\n}\n",
		"Alphasfile.b": "component \"b\" {\n  import \"./Alphasfile.c\" { components = [\"c\"] }\n}\n",
		"Alphasfile.c": `component "c" {}`,
	})
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tree.Files()); n != 4 {
		t.Errorf("want 4 files loaded once, got %v", tree.Files())
	}
	c := 0
	for _, e := range tree.Imports() {
		if strings.HasSuffix(e.Path, "Alphasfile.c") {
			c++
		}
	}
	if c != 1 {
		t.Errorf("Alphasfile.c edge listed %d times: %+v", c, tree.Imports())
	}
}

func TestLoadTree_loadsOnce_cycle(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   `import "./Alphasfile.a" { components = ["a"] }`,
		"Alphasfile.a": "component \"a\" {\n  import \"./Alphasfile.b\" { components = [\"b\"] }\n}\n",
		"Alphasfile.b": "component \"b\" {\n  import \"./Alphasfile.a\" { components = [\"a\"] }\n}\n",
	})
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatalf("a cycle between fragments is not an error: %v", err)
	}
	if n := len(tree.Files()); n != 3 {
		t.Errorf("Files = %v", tree.Files())
	}
}

func TestLoadTree_rejectsEntrypointImport(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":     `import "./svc/Alphasfile" { components = ["svc"] }`,
		"svc/Alphasfile": `component "svc" {}`,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "entrypoints and form federation levels") || !strings.Contains(err.Error(), "Alphasfile.<name>") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_missingImportNamesImporter(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "./nope/Alphasfile.nope" { components = ["x"] }`})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), root+":1") || !strings.Contains(err.Error(), "nope/Alphasfile.nope") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_importErrors(t *testing.T) {
	cases := map[string]struct {
		entry, fragment, want string
	}{
		"empty modules":            {`import "./Alphasfile.f" { components = [] }`, `component "a" {}`, "components must name at least one component"},
		"missing modules":          {`import "./Alphasfile.f" {}`, `component "a" {}`, `"components" is required`},
		"unknown module":           {`import "./Alphasfile.f" { components = ["zz"] }`, "component \"a\" {}\ncomponent \"b\" {}\n", `component "zz" is not declared in`},
		"inputs on a fragment":     {"import \"./Alphasfile.f\" {\n  components = [\"a\"]\n  inputs  = { x = 1 }\n}\n", `component "a" {}`, "inputs and features are passed to a package directory, not to a fragment file"},
		"features on a fragment":   {"import \"./Alphasfile.f\" {\n  components  = [\"a\"]\n  features = [\"x\"]\n}\n", `component "a" {}`, "inputs and features are passed to a package directory, not to a fragment file"},
		"alias on a fragment":      {`import "./Alphasfile.f" "x" { components = ["a"] }`, `component "a" {}`, "an alias names a package; a fragment's components keep their declared names"},
		"package file as fragment": {`import "./Alphasfile.f" { components = ["a"] }`, `package "p" {}`, "is a package's file; import its directory instead"},
		"git import":               {"import \"./Alphasfile.f\" {\n  components = [\"a\"]\n  git { url = \"github.com/x/y\" }\n}\n", `component "a" {}`, "remote imports (git {}) are not supported yet"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": c.entry, "Alphasfile.f": c.fragment})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `import "./Alphasfile.f" { components = ["zz"] }`, "Alphasfile.f": "component \"a\" {}\ncomponent \"b\" {}\n"})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "declared: a, b") {
		t.Errorf("unknown module error must list what the file declares, got %v", err)
	}
}

func TestLoadTree_duplicateModuleAcrossFilesNamesBoth(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   "import \"./Alphasfile.a\" { components = [\"m\"] }\nimport \"./Alphasfile.b\" { components = [\"n\"] }\n",
		"Alphasfile.a": `component "m" {}`,
		"Alphasfile.b": "component \"m\" {}\ncomponent \"n\" {}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `duplicate component "m"`) || !strings.Contains(err.Error(), "Alphasfile.a:1") || !strings.Contains(err.Error(), "Alphasfile.b:1") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_rejectsTopLevelInFragment(t *testing.T) {
	cases := map[string]struct{ fragment, want string }{
		"service":   {"service \"go\" \"x\" {\n  git { url = \"github.com/x/x\" }\n}\n", "top-level service"},
		"toolchain": {"toolchain {\n  go { version = \"1.27.0\" }\n}\n", "top-level toolchain"},
		"env":       {`env = { A = "1" }`, "top-level env"},
		"dotenv":    {`dotenv = ".env"`, "top-level dotenv"},
		"workspace": {"workspace {\n  branch = \"x/${service.name}\"\n}\n", "top-level workspace"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{
				"Alphasfile":   `import "./Alphasfile.f" { components = ["m"] }`,
				"Alphasfile.f": c.fragment + "\ncomponent \"m\" {}\n",
			})
			_, err := LoadTree(root)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Alphasfile.f:") {
				t.Fatalf("want %q naming the fragment, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_rejectsSysenvInFragment(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "./Alphasfile.f" { components = ["m"] }`,
		"Alphasfile.f": "sysenv = [\"TZ\"]\ncomponent \"m\" {}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "Alphasfile.f:1") || !strings.Contains(err.Error(), "top-level sysenv is only allowed in the entrypoint") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_unusedModule(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   `import "./Alphasfile.f" { components = ["used"] }`,
		"Alphasfile.f": "component \"used\" {}\ncomponent \"spare\" {}\n",
	})
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	u := tree.Unused()
	if len(u) != 1 || u[0].Module != "spare" || u[0].Path != filepath.Join(dir, "Alphasfile.f") {
		t.Errorf("Unused = %+v", u)
	}
	if len(tree.modules) != 1 || tree.modules[0].Name != "used" {
		t.Errorf("only the imported module is instantiated, got %d", len(tree.modules))
	}
}

func TestLoadTree_bytesSingleFileEqualsSource(t *testing.T) {
	src := "service \"go\" \"a\" {\n  git { url = \"github.com/x/a\" }\n}\n"
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": src})
	tree, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tree.Bytes(), []byte(src)) {
		t.Errorf("a manifest without imports must hash exactly like before")
	}
}

func TestLoadTree_bytesChangeWhenPackageChanges(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   "import \"./p\" {}\n",
		"p/Alphasfile": `package "p" {}`,
	})
	before, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"p/Alphasfile": "package \"p\" {\n  component \"m\" {}\n}\n"})
	after, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.ConfigHash(before.Bytes(), nil) == invocation.ConfigHash(after.Bytes(), nil) {
		t.Error("editing a package's Alphasfile must change the manifest hash")
	}
}

func TestLoadTree_bytesChangeWhenFragmentChanges(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   `import "./Alphasfile.f" { components = ["m"] }`,
		"Alphasfile.f": `component "m" {}`,
	})
	before, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"Alphasfile.f": "component \"m\" {}\ncomponent \"n\" {}\n"})
	after, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.ConfigHash(before.Bytes(), nil) == invocation.ConfigHash(after.Bytes(), nil) {
		t.Error("editing a fragment must change the manifest hash")
	}
}

func TestLoadTree_fragmentParseErrorNamesFragment(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   `import "./Alphasfile.f" { components = ["m"] }`,
		"Alphasfile.f": "component \"m\" {\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "Alphasfile.f")+":") {
		t.Fatalf("got %v", err)
	}
}

func TestParseTree_rejectsImport(t *testing.T) {
	_, err := ParseTree("test.hcl", []byte(`import "./Alphasfile.f" { components = ["m"] }`))
	if err == nil || !strings.Contains(err.Error(), "imports need a file on disk") {
		t.Fatalf("got %v", err)
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := zfs.EnsureDir(filepath.Dir(p)); err != nil {
			t.Fatal(err)
		}
		if err := zfs.AtomicWrite(p, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "Alphasfile")
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
