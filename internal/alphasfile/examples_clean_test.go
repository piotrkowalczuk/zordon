package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// TestExampleCleanResolves is the resolution oracle for examples/clean: each
// provision carries both a `cmd` (bringup) and a `clean` (teardown), and the
// clean snippets interpolate against the service's own env. Pure Compile — no
// spawn.
func TestExampleCleanResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/clean/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/clean/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/clean/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	app := svcByName(af, "app")
	if app == nil {
		t.Fatal("service app not resolved")
	}

	// fs::state() bakes the invocation's StateDir to an absolute path at
	// eval time, so clean snippets carry no runtime env dependency.
	state := iv.StateDir
	for _, tc := range []struct {
		prov      string
		wantClean string
	}{
		{"seed", "rm -rf " + state + "/data"},
		{"register", "rm -f " + state + "/registered"},
	} {
		p := provByName(app, tc.prov)
		if p == nil {
			t.Fatalf("provision %q not resolved", tc.prov)
		}
		if p.Cmd == "" {
			t.Errorf("provision %q: empty cmd", tc.prov)
		}
		if p.Clean != tc.wantClean {
			t.Errorf("provision %q clean = %q; want %q", tc.prov, p.Clean, tc.wantClean)
		}
		if strings.Contains(p.Clean, "${") {
			t.Errorf("provision %q clean not interpolated: %q", tc.prov, p.Clean)
		}
	}
}
