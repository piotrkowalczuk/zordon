package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
)

func TestSplitIdentity(t *testing.T) {
	cases := map[string]struct {
		id, repo, sub, ref string
		err                string
	}{
		"repo only":          {id: "github.com/acme/infra", repo: "github.com/acme/infra"},
		"subpath and tag":    {id: "github.com/acme/infra/stacks/kafka@v1.2.0", repo: "github.com/acme/infra", sub: "stacks/kafka", ref: "v1.2.0"},
		"branch with slash":  {id: "gitlab.com/a/b/c@feature/x", repo: "gitlab.com/a/b", sub: "c", ref: "feature/x"},
		"empty version":      {id: "github.com/acme/infra@", err: "empty version"},
		"unknown host":       {id: "example.com/a/b", err: "unsupported git host"},
		"too short":          {id: "github.com/acme", err: "too short"},
		"trailing separator": {id: "github.com/acme/infra/stacks/", repo: "github.com/acme/infra", sub: "stacks"},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			repo, sub, ref, err := SplitIdentity(c.id)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("want error %q, got %v", c.err, err)
				}
				return
			}
			if err != nil || repo != c.repo || sub != c.sub || ref != c.ref {
				t.Errorf("SplitIdentity(%q) = %q, %q, %q, %v", c.id, repo, sub, ref, err)
			}
		})
	}
}

func TestSplitIdentity_staysInsideItsRepository(t *testing.T) {
	cases := []string{
		"github.com/acme/infra/../../etc",
		"github.com/acme/infra/./x",
		"github.com/../infra",
		"github.com/acme//infra",
		"github.com/acme/infra/a\\b",
		"github.com/acme/infra@-x",
	}
	for _, id := range cases {
		if _, _, _, err := SplitIdentity(id); err == nil {
			t.Errorf("SplitIdentity(%q) accepted it", id)
		}
	}
}

func TestCheckRef(t *testing.T) {
	cases := map[string]bool{
		"main":                  true,
		"v1.2.0":                true,
		"feat/packages":         true,
		"--upload-pack=touch x": false,
		"-x":                    false,
		"a b":                   false,
		"a\nb":                  false,
		"":                      false,
	}
	for ref, ok := range cases {
		if err := CheckRef(ref); (err == nil) != ok {
			t.Errorf("CheckRef(%q) = %v, want ok=%v", ref, err, ok)
		}
	}
}

func TestIsCommit(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("a", 40):       true,
		strings.Repeat("0", 64):       true,
		strings.Repeat("a", 39):       false,
		strings.Repeat("A", 40):       false,
		"../../../../etc":             false,
		"-" + strings.Repeat("a", 39): false,
	}
	for s, ok := range cases {
		if IsCommit(s) != ok {
			t.Errorf("IsCommit(%q) = %v, want %v", s, !ok, ok)
		}
	}
}

func TestModFetcher(t *testing.T) {
	upstream := taggedRepo(t)
	gitHome := t.TempDir()
	cfg := "[url \"file://" + upstream + "\"]\n\tinsteadOf = https://github.com/acme/infra.git\n"
	if err := zfs.AtomicWrite(filepath.Join(gitHome, ".gitconfig"), []byte(cfg)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", gitHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(gitHome, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	home := t.TempDir()
	f := ModFetcher{Home: home, Keychain: zlock.NewGlobalKeychain(home)}
	commit, err := f.Resolve("github.com/acme/infra", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if want := revParse(t, upstream, "v1"); commit != want {
		t.Fatalf("Resolve = %q, want %q", commit, want)
	}
	dest := filepath.Join(t.TempDir(), "checkout")
	if err := f.Materialize("github.com/acme/infra", commit, dest); err != nil {
		t.Fatal(err)
	}
	if got := revParse(t, dest, "HEAD"); got != commit {
		t.Errorf("checkout HEAD = %q, want %q", got, commit)
	}
	if body, err := zfs.Read(filepath.Join(dest, "README.md")); err != nil || strings.TrimSpace(string(body)) != "one" {
		t.Errorf("README.md = %q, %v; want the v1 content", body, err)
	}
	if err := f.Materialize("github.com/acme/infra", "v1", filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "is not a commit hash") {
		t.Errorf("Materialize with a ref: %v", err)
	}
	if _, err := f.Resolve("github.com/acme/infra", "nope"); err == nil || !strings.Contains(err.Error(), `no branch, tag or commit "nope"`) {
		t.Errorf("Resolve of an unknown ref: %v", err)
	}
}

func TestModFetcher_concurrent(t *testing.T) {
	upstream := taggedRepo(t)
	gitHome := t.TempDir()
	cfg := "[url \"file://" + upstream + "\"]\n\tinsteadOf = https://github.com/acme/infra.git\n"
	if err := zfs.AtomicWrite(filepath.Join(gitHome, ".gitconfig"), []byte(cfg)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", gitHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(gitHome, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	home := t.TempDir()
	f := ModFetcher{Home: home, Keychain: zlock.NewGlobalKeychain(home)}
	dest := filepath.Join(t.TempDir(), "checkout")
	want := revParse(t, upstream, "v1")
	errs := make(chan error, 6)
	for range 6 {
		go func() {
			commit, err := f.Resolve("github.com/acme/infra", "v1")
			if err == nil && commit != want {
				err = fmt.Errorf("Resolve = %q, want %q", commit, want)
			}
			if err == nil {
				err = f.Materialize("github.com/acme/infra", commit, dest)
			}
			errs <- err
		}()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Errorf("two zordons fetching one repository at once must both succeed: %v", err)
		}
	}
	if got := revParse(t, dest, "HEAD"); got != want {
		t.Errorf("checkout HEAD = %q, want %q", got, want)
	}
}

func TestPrimary_ResolveCommit_rejectsAnOption(t *testing.T) {
	p := mustDirPrimary(t, taggedRepo(t), "")
	if _, err := p.ResolveCommit(t.Context(), "--output=x"); err == nil || !strings.Contains(err.Error(), "cannot start with -") {
		t.Fatalf("got %v", err)
	}
}

func TestPrimary_ResolveCommit(t *testing.T) {
	repo := taggedRepo(t)
	p := mustDirPrimary(t, repo, "")
	v1 := revParse(t, repo, "v1")
	v2 := revParse(t, repo, "v2")
	cases := map[string]struct{ ref, want string }{
		"tag":    {"v1", v1},
		"branch": {"main", v2},
		"commit": {v1[:12], v1},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := p.ResolveCommit(t.Context(), c.ref)
			if err != nil || got != c.want {
				t.Errorf("ResolveCommit(%q) = %q, %v; want %q", c.ref, got, err, c.want)
			}
		})
	}
	if _, err := p.ResolveCommit(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), `no branch, tag or commit "nope"`) {
		t.Errorf("unknown ref: %v", err)
	}
}

func revParse(t *testing.T, repo, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", ref+"^{commit}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCountBehind(t *testing.T) {
	repo := taggedRepo(t)
	v1 := revParse(t, repo, "v1")
	head := revParse(t, repo, "main")
	cases := map[string]struct {
		ref, commit string
		want        int
	}{
		"behind":  {"main", v1, 1},
		"current": {"main", head, 0},
		"HEAD":    {"HEAD", v1, 1},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := countBehind(t.Context(), repo, c.ref, c.commit)
			if err != nil || got != c.want {
				t.Fatalf("countBehind(%s, %s) = %d, %v; want %d", c.ref, c.commit, got, err, c.want)
			}
		})
	}
}

func TestModFetcher_Behind_rejectsAnAbbreviatedCommit(t *testing.T) {
	if _, _, err := (ModFetcher{Home: t.TempDir()}).Behind(t.Context(), "github.com/acme/ui", "main", "abc123"); err == nil || !strings.Contains(err.Error(), "is not a commit hash") {
		t.Fatalf("got %v", err)
	}
}

func TestModFetcher_Behind_neverFetched(t *testing.T) {
	_, _, err := (ModFetcher{Home: t.TempDir()}).Behind(t.Context(), "github.com/acme/ui", "main", strings.Repeat("a", 40))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want fs.ErrNotExist: counting must not go to the network", err)
	}
}

func TestLastFetch(t *testing.T) {
	bare := t.TempDir()
	if _, err := lastFetch(bare); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a bare without packed-refs or FETCH_HEAD: got %v", err)
	}
	stamp := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"packed-refs", "FETCH_HEAD"} {
		path := filepath.Join(bare, name)
		if err := zfs.AtomicWrite(path, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		stamp = stamp.Add(time.Hour)
	}
	got, err := lastFetch(bare)
	if err != nil || !got.Equal(stamp.Add(-time.Hour)) {
		t.Fatalf("got %v, %v; want FETCH_HEAD's time", got, err)
	}
}
