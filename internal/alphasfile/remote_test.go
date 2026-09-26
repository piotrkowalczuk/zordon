package alphasfile

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

const remoteRepo = "github.com/acme/infra"

func TestLoadTreeWith_remoteImportFetchesAndLocks(t *testing.T) {
	infra, main := infraRepo(t)
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	f := newRepoFetcher(map[string]string{remoteRepo: infra})
	tree, err := LoadTreeWith(root, remoteOpts(t, dir, f))
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(resolveTree(t, tree)); !equalStrs(got, []string{"web/web"}) {
		t.Errorf("services = %v", got)
	}
	lock := readFile(t, filepath.Join(dir, LockFileName))
	if !strings.Contains(lock, `repo "github.com/acme/infra"`) || !strings.Contains(lock, `commit = "`+main+`"`) || !strings.Contains(lock, `ref    = "main"`) {
		t.Errorf("lock file:\n%s", lock)
	}
	if ch := tree.LockChanges(); len(ch) != 1 || ch[0].New != main || ch[0].Old != "" {
		t.Errorf("changes = %+v", ch)
	}
	if !strings.Contains(string(tree.Bytes()), remoteRepo+"@"+main+"//pkgs/web/Alphasfile") {
		t.Error("the config hash must record the remote identity and commit")
	}
	if edges := tree.Imports(); len(edges) != 1 || edges[0].Origin != remoteRepo+"@"+main[:12] {
		t.Errorf("edges = %+v", edges)
	}
}

func TestLoadTreeWith_lockedRepoLoadsOffline(t *testing.T) {
	infra, main := infraRepo(t)
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	opts := remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra}))
	if _, err := LoadTreeWith(root, opts); err != nil {
		t.Fatal(err)
	}
	commitFile(t, infra, "pkgs/web/NOTE", "moved on")
	opts.Fetcher = nil
	tree, err := LoadTreeWith(root, opts)
	if err != nil {
		t.Fatalf("a locked, checked-out repository needs no fetch: %v", err)
	}
	if !strings.Contains(string(tree.Bytes()), remoteRepo+"@"+main) {
		t.Error("the lock must pin the old commit even though main moved")
	}
}

func TestLoadTreeWith_updateMovesTheLock(t *testing.T) {
	infra, main := infraRepo(t)
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	opts := remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra}))
	if _, err := LoadTreeWith(root, opts); err != nil {
		t.Fatal(err)
	}
	next := commitFile(t, infra, "pkgs/web/NOTE", "moved on")
	opts.UpdateAll = true
	tree, err := LoadTreeWith(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ch := tree.LockChanges(); len(ch) != 1 || ch[0].Old != main || ch[0].New != next {
		t.Errorf("changes = %+v, want %s -> %s", ch, main, next)
	}
	if lock := readFile(t, filepath.Join(dir, LockFileName)); !strings.Contains(lock, next) {
		t.Errorf("lock file:\n%s", lock)
	}
}

func TestLoadTreeWith_changedRefResolvesAgain(t *testing.T) {
	infra, _ := infraRepo(t)
	v1 := gitOut(t, infra, "rev-parse", "v1^{commit}")
	commitFile(t, infra, "pkgs/web/NOTE", "after v1")
	dir := t.TempDir()
	opts := remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra}))
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	if _, err := LoadTreeWith(root, opts); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@v1" {}`})
	if _, err := LoadTreeWith(root, opts); err != nil {
		t.Fatal(err)
	}
	lock := readFile(t, filepath.Join(dir, LockFileName))
	if !strings.Contains(lock, `ref    = "v1"`) || !strings.Contains(lock, v1) {
		t.Errorf("an import that names another version re-resolves; lock:\n%s", lock)
	}
}

func TestLoadTreeWith_oneVersionPerRepository(t *testing.T) {
	infra, _ := infraRepo(t)
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{
		"Alphasfile":     "import \"github.com/acme/infra/pkgs/web@main\" {}\nimport \"./app\" {}\n",
		"app/Alphasfile": `require "github.com/acme/infra/pkgs/db@v1" {}`,
	})
	_, err := LoadTreeWith(root, remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra})))
	if err == nil || !strings.Contains(err.Error(), "one stack uses one version of a repository") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTreeWith_remoteImportNeedsVersion(t *testing.T) {
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web" {}`})
	_, err := LoadTreeWith(root, remoteOpts(t, dir, newRepoFetcher(nil)))
	if err == nil || !strings.Contains(err.Error(), "a remote import needs a version: add @<branch, tag or commit>") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTreeWith_searchByZordonMod(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout, map[string]string{
		ModFileName:           `module = "github.com/acme/infra"`,
		"pkgs/web/Alphasfile": pkgWeb,
	})
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	opts := remoteOpts(t, dir, nil)
	opts.Search = []string{checkout}
	tree, err := LoadTreeWith(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(resolveTree(t, tree)); !equalStrs(got, []string{"web/web"}) {
		t.Errorf("services = %v", got)
	}
	if edges := tree.Imports(); len(edges) != 1 || edges[0].Origin != "search "+checkout {
		t.Errorf("edges = %+v", edges)
	}
	if zfs.Exists(filepath.Join(dir, LockFileName)) {
		t.Error("a repository provided by search is not locked")
	}
}

func TestLoadTreeWith_searchByLayout(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"github.com/acme/infra/pkgs/web/Alphasfile": pkgWeb})
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	opts := remoteOpts(t, dir, nil)
	opts.Search = []string{src}
	tree, err := LoadTreeWith(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(resolveTree(t, tree)); !equalStrs(got, []string{"web/web"}) {
		t.Errorf("services = %v", got)
	}
}

func TestLoadTreeWith_searchDuplicateProvider(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		writeTree(t, d, map[string]string{ModFileName: `module = "github.com/acme/infra"`, "pkgs/web/Alphasfile": pkgWeb})
	}
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	opts := remoteOpts(t, dir, nil)
	opts.Search = []string{a, b}
	_, err := LoadTreeWith(root, opts)
	if err == nil || !strings.Contains(err.Error(), "is provided by 2 search entries in zordon.work") || !strings.Contains(err.Error(), "keep one of them") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTreeWith_remoteFilesStayInsideTheirCheckout(t *testing.T) {
	cases := map[string]struct{ require, want string }{
		"escape":   {`require "../../../../outside" {}`, "leaves the checkout of github.com/acme/infra@"},
		"absolute": {`require "/etc" {}`, "a file fetched from a remote repository may import relative paths only"},
		"home":     {`require "~/x" {}`, "a file fetched from a remote repository may import relative paths only"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			infra := gitRepo(t, map[string]string{"pkgs/web/Alphasfile": c.require + "\n" + pkgWeb})
			dir := t.TempDir()
			root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
			_, err := LoadTreeWith(root, remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra})))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTreeWith_remoteRelativeRequireInsideCheckout(t *testing.T) {
	infra := gitRepo(t, map[string]string{
		"pkgs/web/Alphasfile": "require \"../db\" {}\n" + pkgWeb,
		"pkgs/db/Alphasfile":  "service \"go\" \"db\" {\n  git { url = \"github.com/x/db\" }\n}\n",
	})
	main := gitOut(t, infra, "rev-parse", "HEAD")
	dir := t.TempDir()
	root := writeTree(t, dir, map[string]string{"Alphasfile": `import "github.com/acme/infra/pkgs/web@main" {}`})
	tree, err := LoadTreeWith(root, remoteOpts(t, dir, newRepoFetcher(map[string]string{remoteRepo: infra})))
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceNames(resolveTree(t, tree)); !equalStrs(got, []string{"web/web", "db/db"}) {
		t.Errorf("services = %v", got)
	}
	if !strings.Contains(string(tree.Bytes()), remoteRepo+"@"+main+"//pkgs/db/Alphasfile") {
		t.Error("a relative file inside a checkout keeps the remote identity in the hash")
	}
}

func TestReadWorkFile(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"code/infra/.keep": "", "zordon.work": "search \"./code/infra\" {}\n"})
	got, err := ReadWorkFile(filepath.Join(dir, "zordon.work"))
	if err != nil || !equalStrs(got, []string{filepath.Join(dir, "code", "infra")}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestReadWorkFile_errors(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"missing dir":   {`search "./nope" {}`, "is not a directory"},
		"repeated":      {"search \"./code\" {}\nsearch \"./code/\" {}\n", "repeats the entry at"},
		"unknown field": {`project "./code" {}`, `Blocks of type "project" are not expected here`},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"code/.keep": "", "zordon.work": c.body + "\n"})
			if _, err := ReadWorkFile(filepath.Join(dir, "zordon.work")); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestReadMod_rejectsAnInvalidModule(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{ModFileName: `module = "example.com/x/y"`})
	if _, err := readMod(dir); err == nil || !strings.Contains(err.Error(), "unsupported git host") {
		t.Fatalf("got %v", err)
	}
}

// repoFetcher serves repository identities from local git repositories, so
// remote resolution is exercised without the network.
type repoFetcher struct {
	repos    map[string]string
	resolves int
}

func newRepoFetcher(repos map[string]string) *repoFetcher {
	return &repoFetcher{repos: repos}
}

func (f *repoFetcher) Resolve(repo, ref string) (string, error) {
	f.resolves++
	local, ok := f.repos[repo]
	if !ok {
		return "", &exec.Error{Name: repo, Err: exec.ErrNotFound}
	}
	out, err := exec.Command("git", "-C", local, "rev-parse", "--verify", ref+"^{commit}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (f *repoFetcher) Materialize(repo, commit, dest string) error {
	if zfs.Exists(dest) {
		return nil
	}
	if out, err := exec.Command("git", "clone", "-q", f.repos[repo], dest).CombinedOutput(); err != nil {
		return &exec.ExitError{Stderr: out}
	}
	return exec.Command("git", "-C", dest, "checkout", "-q", "--detach", commit).Run()
}

func remoteOpts(t *testing.T, dir string, f Fetcher) LoadOptions {
	t.Helper()
	opts := LoadOptions{Home: t.TempDir(), LockPath: filepath.Join(dir, LockFileName)}
	if f != nil {
		opts.Fetcher = f
	}
	return opts
}

// infraRepo is a repository with packages pkgs/web and pkgs/db, tagged v1.
func infraRepo(t *testing.T) (repo, head string) {
	t.Helper()
	repo = gitRepo(t, map[string]string{
		"pkgs/web/Alphasfile": pkgWeb,
		"pkgs/db/Alphasfile":  "service \"go\" \"db\" {\n  git { url = \"github.com/x/db\" }\n}\n",
	})
	gitRun(t, repo, "tag", "v1")
	return repo, gitOut(t, repo, "rev-parse", "HEAD")
}

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	ztest.AssertSystem(t)
	repo := t.TempDir()
	writeTree(t, repo, files)
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	return repo
}

func commitFile(t *testing.T, repo, rel, body string) string {
	t.Helper()
	writeTree(t, repo, map[string]string{rel: body})
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "-c", "commit.gpgsign=false", "commit", "-q", "-m", rel)
	return gitOut(t, repo, "rev-parse", "HEAD")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := zfs.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func resolveTree(t *testing.T, tree *Tree) *Alphasfile {
	t.Helper()
	af, err := Resolve(tree, testInv(), nil, testCfgHash, TestConfig{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return af
}
