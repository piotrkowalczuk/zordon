package alphasfile

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRequire_topLevel(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web" {}` + "\n"})
	got, err := AddRequire(root, "github.com/acme/infra", "main")
	if err != nil || got != root {
		t.Fatalf("got %q, %v", got, err)
	}
	if body := readFile(t, root); !strings.Contains(body, "require \"github.com/acme/infra\" {\n  ref = \"main\"\n}") {
		t.Errorf("Alphasfile:\n%s", body)
	}
}

func TestAddRequire_movesAnExistingRequire(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": "require \"github.com/acme/infra\" { ref = \"main\" }\n"})
	if _, err := AddRequire(root, "github.com/acme/infra", "v2"); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, root)
	if strings.Count(body, "require") != 1 || !strings.Contains(body, `ref = "v2"`) {
		t.Errorf("Alphasfile:\n%s", body)
	}
}

func TestAddRequire_insidePackageBlock(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": pkgWeb})
	if _, err := AddRequire(root, "github.com/acme/infra", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTree(root); err != nil {
		t.Fatalf("the require must land inside the package block: %v\n%s", err, readFile(t, root))
	}
}

func TestAddRequire_zordonMod(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		ModFileName:        `module = "github.com/acme/stack"` + "\n",
		"place/Alphasfile": "",
	})
	got, err := AddRequire(filepath.Join(dir, "place", "Alphasfile"), "github.com/acme/infra", "main")
	if err != nil || got != filepath.Join(dir, ModFileName) {
		t.Fatalf("got %q, %v", got, err)
	}
	if body := readFile(t, got); !strings.Contains(body, `require "github.com/acme/infra"`) {
		t.Errorf("zordon.mod:\n%s", body)
	}
	if body := readFile(t, filepath.Join(dir, "place", "Alphasfile")); body != "" {
		t.Errorf("the Alphasfile under a zordon.mod stays untouched:\n%s", body)
	}
}

func TestAddRequire_errors(t *testing.T) {
	cases := map[string]struct{ repo, ref, want string }{
		"subdirectory": {"github.com/acme/infra/pkgs", "main", "name the repository alone, such as github.com/acme/infra"},
		"no ref":       {"github.com/acme/infra", "", "name a branch, tag or commit, such as github.com/acme/infra@main"},
		"bad host":     {"example.com/a/b", "main", "unsupported git host"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			root := writeTree(t, t.TempDir(), map[string]string{"Alphasfile": ""})
			if _, err := AddRequire(root, c.repo, c.ref); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
