package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

const webPackage = `
package "web" {
  module "web" {
    service "go" "web" {
      git { url = "github.com/x/web" }
    }
  }
}
`

func TestImportLines_package(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Alphasfile":     `import "./web" "edge" { features = ["tls"] }`,
		"web/Alphasfile": "package \"web\" {\n  features = { tls = { description = \"Serves HTTPS\" } }\n}\n",
	})
	tree, err := alphasfile.LoadTree(filepath.Join(dir, "Alphasfile"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# import " + filepath.Join(dir, "web") + " as edge [features: tls]\n"
	if got := importLines("# ", tree); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

const infraWebAtMain = "require \"github.com/acme/infra\" { ref = \"main\" }\nimport \"github.com/acme/infra/pkgs/web\" {}\n"

func TestImportLines_importedBy(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Alphasfile":     "import \"./app\" {}\n",
		"app/Alphasfile": "package \"app\" {\n  import \"../web\" { features = [\"tls\"] }\n}\n",
		"web/Alphasfile": "package \"web\" {\n  features = { tls = { description = \"Serves HTTPS\" } }\n}\n",
	})
	tree, err := alphasfile.LoadTree(filepath.Join(dir, "Alphasfile"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# import " + filepath.Join(dir, "web") + " as web [features: tls] (imported by package app)\n"
	if got := importLines("# ", tree); !strings.Contains(got, want) {
		t.Errorf("got\n%s\nwant a line\n%s", got, want)
	}
}

func TestPkgGet(t *testing.T) {
	ztest.AssertSystem(t)
	infra := t.TempDir()
	writeFiles(t, infra, map[string]string{"pkgs/web/Alphasfile": webPackage})
	gitIn(t, infra, "init", "-q", "-b", "main")
	commitAll(t, infra, "init")
	head := gitHead(t, infra)

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"Alphasfile": "import \"github.com/acme/infra/pkgs/web\" {}\n"})
	af := filepath.Join(dir, "Alphasfile")
	opts := alphasfile.LoadOptions{
		Home:     t.TempDir(),
		LockPath: filepath.Join(dir, alphasfile.LockFileName),
		Fetcher:  localFetcher{"github.com/acme/infra": infra},
	}
	var out bytes.Buffer
	if err := pkgGet(&out, af, opts, "github.com/acme/infra@main"); err != nil {
		t.Fatal(err)
	}
	want := "require \"github.com/acme/infra\" { ref = \"main\" } in " + af + "\ngithub.com/acme/infra@main: (new) -> " + head[:12] + "\n"
	if out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
	if _, err := alphasfile.LoadTreeWith(af, opts); err != nil {
		t.Errorf("after pkg get the Alphasfile loads: %v", err)
	}
}

func TestImportLines_searchOrigin(t *testing.T) {
	checkout := t.TempDir()
	writeFiles(t, checkout, map[string]string{
		"zordon.mod":          `module = "github.com/acme/infra"`,
		"pkgs/web/Alphasfile": webPackage,
	})
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"Alphasfile": infraWebAtMain})
	tree, err := alphasfile.LoadTreeWith(filepath.Join(dir, "Alphasfile"), alphasfile.LoadOptions{Search: []string{checkout}})
	if err != nil {
		t.Fatal(err)
	}
	want := "# import " + filepath.Join(checkout, "pkgs", "web") + " as web (search " + checkout + ")\n"
	if got := importLines("# ", tree); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestUpdateLock(t *testing.T) {
	ztest.AssertSystem(t)
	infra := t.TempDir()
	writeFiles(t, infra, map[string]string{"pkgs/web/Alphasfile": webPackage})
	gitIn(t, infra, "init", "-q", "-b", "main")
	commitAll(t, infra, "init")
	first := gitHead(t, infra)

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"Alphasfile": infraWebAtMain})
	af := filepath.Join(dir, "Alphasfile")
	opts := alphasfile.LoadOptions{
		Home:     t.TempDir(),
		LockPath: filepath.Join(dir, alphasfile.LockFileName),
		Fetcher:  localFetcher{"github.com/acme/infra": infra},
	}

	var out bytes.Buffer
	if err := updateLock(&out, af, opts, nil); err != nil {
		t.Fatal(err)
	}
	if want := "github.com/acme/infra@main: (new) -> " + first[:12] + "\n"; out.String() != want {
		t.Errorf("first update printed %q, want %q", out.String(), want)
	}

	out.Reset()
	if err := updateLock(&out, af, opts, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "zordon.lock is up to date\n" {
		t.Errorf("second update printed %q", out.String())
	}

	writeFiles(t, infra, map[string]string{"pkgs/web/NOTE": "next"})
	commitAll(t, infra, "next")
	next := gitHead(t, infra)
	out.Reset()
	if err := updateLock(&out, af, opts, []string{"github.com/acme/infra"}); err != nil {
		t.Fatal(err)
	}
	if want := "github.com/acme/infra@main: " + first[:12] + " -> " + next[:12] + "\n"; out.String() != want {
		t.Errorf("update after a push printed %q, want %q", out.String(), want)
	}
}

// localFetcher serves repository identities from local git repositories.
type localFetcher map[string]string

func (f localFetcher) Resolve(repo, ref string) (string, error) {
	out, err := exec.Command("git", "-C", f[repo], "rev-parse", "--verify", ref+"^{commit}").Output()
	return strings.TrimSpace(string(out)), err
}

func (f localFetcher) Materialize(repo, commit, dest string) error {
	if zfs.Exists(dest) {
		return nil
	}
	if err := exec.Command("git", "clone", "-q", f[repo], dest).Run(); err != nil {
		return err
	}
	return exec.Command("git", "-C", dest, "checkout", "-q", "--detach", commit).Run()
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
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
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "user.email=a@b", "-c", "user.name=z", "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
