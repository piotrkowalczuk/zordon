package main

import (
	"path/filepath"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

func TestImportLines(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"Alphasfile":   `import "Alphasfile.f" { modules = ["a"] }`,
		"Alphasfile.f": "module \"a\" {}\nmodule \"b\" {}\n",
	} {
		if err := zfs.AtomicWrite(filepath.Join(dir, rel), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := alphasfile.LoadTree(filepath.Join(dir, "Alphasfile"))
	if err != nil {
		t.Fatal(err)
	}
	frag := filepath.Join(dir, "Alphasfile.f")
	want := "# import " + frag + " [a]\n# unused module b in " + frag + "\n"
	if got := importLines("# ", tree); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestImportLines_noImports(t *testing.T) {
	tree, err := alphasfile.ParseTree("Alphasfile", []byte(`module "a" {}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := importLines("# ", tree); got != "" {
		t.Errorf("got %q", got)
	}
	if got := importLines("# ", nil); got != "" {
		t.Errorf("nil tree: got %q", got)
	}
}
