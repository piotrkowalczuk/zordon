package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// examples/workspace is a monorepo: three services (serviceA, serviceB,
// serviceC) share one primary (src = ../..). Oracle covers both
// workspace modes. Pure Compile.
func TestExampleWorkspaceMonorepo(t *testing.T) {
	b, err := os.ReadFile("../../examples/workspace/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"serviceA", "serviceB", "serviceC"}
	compile := func(wt, stateDir string) []*Service {
		iv := &invocation.InvocationState{
			FsHash: "abc0000011112222", TmpDir: "/tmp/zordon-abc0000011112222",
			Workspace: wt, StateDir: stateDir,
		}
		af, err := Compile("/repo/examples/workspace/Alphasfile", b, iv, nil, "", TestConfig{})
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
	for i, s := range compile(invocation.MainWorkspace,
		"/repo/examples/workspace/workspaces/main") {
		want := "/repo/examples/workspace/src/" + names[i]
		if s == nil || !s.Package.InPlace || s.Runtime.Dir != want {
			t.Fatalf("main: %s must be in-place @ %s: got dir %q, %+v",
				s.Name(), want, s.Runtime.Dir, s.Package)
		}
		if !strings.Contains(s.Runtime.Print, "127.0.0.1:") {
			t.Errorf("%s print not resolved: %q", s.Name(), s.Runtime.Print)
		}
	}

	// named workspace → all workspace-able, NOT in-place. Each gets its
	// OWN checkout dir (monorepo branch/dir is per-service), and dir is
	// the exe-anchored work dir = <checkout>/<exe>.
	svcs := compile("feature",
		"/repo/examples/workspace/workspaces/feature")
	seen := map[string]bool{}
	for i, s := range svcs {
		if s.Package.InPlace {
			t.Fatalf("named workspace %s must not be in-place", s.Name())
		}
		if !s.Workspaceable() {
			t.Fatalf("%s must be workspace-able", s.Name())
		}
		want := "/repo/examples/workspace/workspaces/feature/src/" + names[i] +
			"/examples/workspace/src/" + names[i]
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
