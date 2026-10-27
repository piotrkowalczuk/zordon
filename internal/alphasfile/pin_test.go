package alphasfile

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPinSources_locksTheCommitTheRefPointsAt(t *testing.T) {
	repo := gitRepo(t, map[string]string{"README": "ui"})
	head := gitOut(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "tag", "v1")
	f := newRepoFetcher(map[string]string{"github.com/acme/ui": repo})
	dir := t.TempDir()
	af := &Alphasfile{Services: []*Service{
		pinSvc("merchant", &Package{Git: "github.com/acme/ui"}),
		pinSvc("buyer", &Package{Git: "github.com/acme/ui"}),
		pinSvc("main", &Package{Git: "github.com/acme/ui", Branch: "main"}),
		pinSvc("tagged", &Package{Git: "github.com/acme/ui", Tag: "v1"}),
		pinSvc("picked", &Package{Git: "github.com/acme/ui", Editable: true}),
	}}

	changes, pins, err := PinSources(af, remoteOpts(t, dir, f), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"merchant", "buyer", "main"} {
		if got := pinnedTo(af, name); got != head {
			t.Errorf("%s locked at %q, want %s", name, got, head)
		}
	}
	for _, name := range []string{"tagged", "picked"} {
		if got := pinnedTo(af, name); got != "" {
			t.Errorf("%s locked at %q, want nothing", name, got)
		}
	}
	if f.resolves != 2 {
		t.Errorf("resolved %d times, want once per repository and ref", f.resolves)
	}
	if len(changes) != 2 || len(pins) != 2 {
		t.Fatalf("changes %v, pins %v; want HEAD and main of github.com/acme/ui", changes, pins)
	}
	lock := readFile(t, filepath.Join(dir, LockFileName))
	for _, want := range []string{
		"source \"github.com/acme/ui\" {\n  ref    = \"HEAD\"\n  commit = \"" + head + "\"\n}",
		"source \"github.com/acme/ui\" {\n  ref    = \"main\"\n  commit = \"" + head + "\"\n}",
	} {
		if !strings.Contains(lock, want) {
			t.Errorf("lock misses %q:\n%s", want, lock)
		}
	}
}

func TestPinSources_keepsTheLockedCommit(t *testing.T) {
	repo := gitRepo(t, map[string]string{"README": "ui"})
	old := gitOut(t, repo, "rev-parse", "HEAD")
	f := newRepoFetcher(map[string]string{"github.com/acme/ui": repo})
	opts := remoteOpts(t, t.TempDir(), f)
	if _, _, err := PinSources(&Alphasfile{Services: []*Service{pinSvc("ui", &Package{Git: "github.com/acme/ui"})}}, opts, false); err != nil {
		t.Fatal(err)
	}
	commitFile(t, repo, "CHANGELOG", "newer")

	af := &Alphasfile{Services: []*Service{pinSvc("ui", &Package{Git: "github.com/acme/ui"})}}
	changes, _, err := PinSources(af, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinnedTo(af, "ui"); got != old || len(changes) != 0 || f.resolves != 1 {
		t.Errorf("locked at %s with changes %v after %d resolves; want %s untouched, without fetching", got, changes, f.resolves, old)
	}
}

func TestPinSources_pullMovesToTheNewestCommit(t *testing.T) {
	repo := gitRepo(t, map[string]string{"README": "ui"})
	old := gitOut(t, repo, "rev-parse", "HEAD")
	f := newRepoFetcher(map[string]string{"github.com/acme/ui": repo})
	opts := remoteOpts(t, t.TempDir(), f)
	if _, _, err := PinSources(&Alphasfile{Services: []*Service{pinSvc("ui", &Package{Git: "github.com/acme/ui"})}}, opts, false); err != nil {
		t.Fatal(err)
	}
	head := commitFile(t, repo, "CHANGELOG", "newer")

	af := &Alphasfile{Services: []*Service{pinSvc("ui", &Package{Git: "github.com/acme/ui"})}}
	changes, _, err := PinSources(af, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinnedTo(af, "ui"); got != head {
		t.Errorf("locked at %s, want %s", got, head)
	}
	if len(changes) != 1 || changes[0].Old != old || changes[0].New != head {
		t.Errorf("changes = %+v, want %s -> %s", changes, old, head)
	}
}

func TestPinSources_keepsTheRepositoryPins(t *testing.T) {
	repo := gitRepo(t, map[string]string{"README": "ui"})
	dir := t.TempDir()
	modules := "repo \"github.com/acme/infra\" {\n  ref    = \"main\"\n  commit = \"" + strings.Repeat("a", 40) + "\"\n}\n"
	writeTree(t, dir, map[string]string{LockFileName: modules})
	af := &Alphasfile{Services: []*Service{pinSvc("ui", &Package{Git: "github.com/acme/ui"})}}

	if _, _, err := PinSources(af, remoteOpts(t, dir, newRepoFetcher(map[string]string{"github.com/acme/ui": repo})), false); err != nil {
		t.Fatal(err)
	}
	if lock := readFile(t, filepath.Join(dir, LockFileName)); !strings.Contains(lock, modules) {
		t.Errorf("pinning the sources dropped the repository pins:\n%s", lock)
	}
}

func TestLoadTreeWith_keepsTheSourcePins(t *testing.T) {
	infra, head := infraRepo(t)
	dir := t.TempDir()
	sources := "source \"github.com/acme/ui\" {\n  ref    = \"HEAD\"\n  commit = \"" + strings.Repeat("b", 40) + "\"\n}\n"
	writeTree(t, dir, map[string]string{
		"Alphasfile": "require \"github.com/acme/infra\" { ref = \"main\" }\nimport \"github.com/acme/infra/pkgs/web\" {}\n",
		LockFileName: sources,
	})

	tree, err := LoadTreeWith(filepath.Join(dir, "Alphasfile"), remoteOpts(t, dir, newRepoFetcher(map[string]string{"github.com/acme/infra": infra})))
	if err != nil {
		t.Fatal(err)
	}
	if lock := readFile(t, filepath.Join(dir, LockFileName)); !strings.Contains(lock, sources) || !strings.Contains(lock, head) {
		t.Errorf("locking the repositories dropped the source pins:\n%s", lock)
	}
	if pins := tree.Pins(); len(pins) != 1 || pins[0] != (LockPin{Repo: "github.com/acme/infra", Ref: "main", Commit: head}) {
		t.Errorf("Pins() = %+v", pins)
	}
}

func pinSvc(name string, p *Package) *Service {
	p.Toolchain = "go"
	return &Service{Toolchain: "go", Package: p, Runtime: &RuntimeConfig{Name: name}}
}

func pinnedTo(af *Alphasfile, name string) string {
	for _, s := range af.Services {
		if s.Name() == name {
			return s.Package.Locked
		}
	}
	return "missing"
}
