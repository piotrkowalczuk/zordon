package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// examples/worktree is a monorepo: three services (serviceA, serviceB,
// serviceC) share one primary (src = ../..). Oracle covers both
// worktree modes. Pure Compile.
func TestExampleWorktreeMonorepo(t *testing.T) {
	b, err := os.ReadFile("../../examples/worktree/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"serviceA", "serviceB", "serviceC"}
	compile := func(wt, stateDir string) []*Service {
		iv := &invocation.Invocation{
			FsHash: "abc0000011112222", TmpDir: "/tmp/zordon-abc0000011112222",
			Worktree: wt, StateDir: stateDir,
		}
		af, err := Compile("/repo/examples/worktree/Alphasfile", b, iv, nil, "", TestConfig{})
		if err != nil {
			t.Fatalf("compile (%s): %v", wt, err)
		}
		out := make([]*Service, len(names))
		for i, n := range names {
			out[i] = svcByName(af, n)
		}
		return out
	}

	// main → all in-place from the live repo, no per-service checkout.
	// Runtime.Dir is the exe-anchored work dir (zfs.ServiceCwd): the
	// resolved src.path (/repo) joined with each service's exe offset.
	for i, s := range compile(invocation.MainWorktree,
		"/repo/examples/worktree/.zordon/worktrees/main") {
		want := "/repo/examples/worktree/src/" + names[i]
		if s == nil || !s.Package.InPlace || s.Runtime.Dir != want {
			t.Fatalf("main: %s must be in-place @ %s: got dir %q, %+v",
				s.Name(), want, s.Runtime.Dir, s.Package)
		}
		if !strings.Contains(s.Runtime.Print, "127.0.0.1:") {
			t.Errorf("%s print not resolved: %q", s.Name(), s.Runtime.Print)
		}
	}

	// named worktree → all worktree-able, NOT in-place. Each gets its
	// OWN checkout dir (monorepo branch/dir is per-service), and dir is
	// the exe-anchored work dir = <checkout>/<exe>.
	svcs := compile("feature",
		"/repo/examples/worktree/.zordon/worktrees/feature")
	seen := map[string]bool{}
	for i, s := range svcs {
		if s.Package.InPlace {
			t.Fatalf("named worktree %s must not be in-place", s.Name())
		}
		if !s.Worktreeable() {
			t.Fatalf("%s must be worktree-able", s.Name())
		}
		want := "/repo/examples/worktree/.zordon/worktrees/feature/src/" + names[i] +
			"/examples/worktree/src/" + names[i]
		if s.Runtime.Dir != want {
			t.Fatalf("%s work dir wrong: got %q want %q",
				s.Name(), s.Runtime.Dir, want)
		}
		if seen[s.Runtime.Dir] {
			t.Fatalf("monorepo services must have distinct work dirs: %q", s.Runtime.Dir)
		}
		seen[s.Runtime.Dir] = true
	}
}
