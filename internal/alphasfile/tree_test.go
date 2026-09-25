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
		"Alphasfile":           `import "a/Alphasfile.a" { modules = ["a"] }`,
		"a/Alphasfile.a":       "module \"a\" {\n  import \"../b/Alphasfile.b\" { modules = [\"b\"] }\n}\n",
		"b/Alphasfile.b":       `module "b" {}`,
		"b/Alphasfile.ignored": `module "x" {}`,
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
		"Alphasfile":   "import \"Alphasfile.a\" { modules = [\"a\"] }\nimport \"Alphasfile.b\" { modules = [\"b\"] }\n",
		"Alphasfile.a": "module \"a\" {\n  import \"Alphasfile.c\" { modules = [\"c\"] }\n}\n",
		"Alphasfile.b": "module \"b\" {\n  import \"Alphasfile.c\" { modules = [\"c\"] }\n}\n",
		"Alphasfile.c": `module "c" {}`,
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
		"Alphasfile":   `import "Alphasfile.a" { modules = ["a"] }`,
		"Alphasfile.a": "module \"a\" {\n  import \"Alphasfile.b\" { modules = [\"b\"] }\n}\n",
		"Alphasfile.b": "module \"b\" {\n  import \"Alphasfile.a\" { modules = [\"a\"] }\n}\n",
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
		"Alphasfile":     `import "svc/Alphasfile" { modules = ["svc"] }`,
		"svc/Alphasfile": `module "svc" {}`,
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), "entrypoints and form federation levels") || !strings.Contains(err.Error(), "Alphasfile.<name>") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_missingImportNamesImporter(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "nope/Alphasfile.nope" { modules = ["x"] }`})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), root+":1") || !strings.Contains(err.Error(), "nope/Alphasfile.nope") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTree_importErrors(t *testing.T) {
	cases := map[string]struct {
		entry, fragment, want string
	}{
		"empty modules":   {`import "Alphasfile.f" { modules = [] }`, `module "a" {}`, "modules must name at least one module"},
		"missing modules": {`import "Alphasfile.f" {}`, `module "a" {}`, `"modules" is required`},
		"unknown module":  {`import "Alphasfile.f" { modules = ["zz"] }`, "module \"a\" {}\nmodule \"b\" {}\n", `module "zz" is not declared in`},
		"git import":      {"import \"Alphasfile.f\" {\n  modules = [\"a\"]\n  git { url = \"github.com/x/y\" }\n}\n", `module "a" {}`, "remote imports (git {}) are not supported yet"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": c.entry, "Alphasfile.f": c.fragment})
			if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": `import "Alphasfile.f" { modules = ["zz"] }`, "Alphasfile.f": "module \"a\" {}\nmodule \"b\" {}\n"})
	if _, err := LoadTree(root); err == nil || !strings.Contains(err.Error(), "declared: a, b") {
		t.Errorf("unknown module error must list what the file declares, got %v", err)
	}
}

func TestLoadTree_duplicateModuleAcrossFilesNamesBoth(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   "import \"Alphasfile.a\" { modules = [\"m\"] }\nimport \"Alphasfile.b\" { modules = [\"n\"] }\n",
		"Alphasfile.a": `module "m" {}`,
		"Alphasfile.b": "module \"m\" {}\nmodule \"n\" {}\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), `duplicate module "m"`) || !strings.Contains(err.Error(), "Alphasfile.a:1") || !strings.Contains(err.Error(), "Alphasfile.b:1") {
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
				"Alphasfile":   `import "Alphasfile.f" { modules = ["m"] }`,
				"Alphasfile.f": c.fragment + "\nmodule \"m\" {}\n",
			})
			_, err := LoadTree(root)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Alphasfile.f:") {
				t.Fatalf("want %q naming the fragment, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTree_fragmentSysenvAllowed(t *testing.T) {
	root := writeTree(t, t.TempDir(), map[string]string{
		"Alphasfile":   `import "Alphasfile.f" { modules = ["m"] }`,
		"Alphasfile.f": "sysenv = [\"TZ\"]\nmodule \"m\" {}\n",
	})
	if _, err := LoadTree(root); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTree_unusedModule(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   `import "Alphasfile.f" { modules = ["used"] }`,
		"Alphasfile.f": "module \"used\" {}\nmodule \"spare\" {}\n",
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

func TestLoadTree_bytesChangeWhenFragmentChanges(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":   `import "Alphasfile.f" { modules = ["m"] }`,
		"Alphasfile.f": `module "m" {}`,
	})
	before, err := LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"Alphasfile.f": "sysenv = [\"TZ\"]\nmodule \"m\" {}\n"})
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
		"Alphasfile":   `import "Alphasfile.f" { modules = ["m"] }`,
		"Alphasfile.f": "module \"m\" {\n",
	})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "Alphasfile.f")+":") {
		t.Fatalf("got %v", err)
	}
}

func TestParseTree_rejectsImport(t *testing.T) {
	_, err := ParseTree("test.hcl", []byte(`import "Alphasfile.f" { modules = ["m"] }`))
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
