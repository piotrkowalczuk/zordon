package source

import (
	"os/exec"
	"strings"
	"testing"
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
