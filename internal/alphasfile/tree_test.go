package alphasfile

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestLoadTree_missingImportNamesImporter(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "./nope/Alphasfile.nope" { components = ["x"] }`})
	_, err := LoadTree(root)
	if err == nil || !strings.Contains(err.Error(), root+":1") || !strings.Contains(err.Error(), "nope/Alphasfile.nope") {
		t.Fatalf("got %v", err)
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
		"Alphasfile":   `component "a" {}`,
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
		t.Error("editing a part of the Alphasfile must change the manifest hash")
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
