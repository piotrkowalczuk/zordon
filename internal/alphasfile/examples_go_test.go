package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the Go toolchain: examples/go must resolve and the
// three source modes (src, git+src, use-only) must produce the right
// facts. Pure Compile — no clone, no spawn.
func TestExampleGoResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/go/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/go/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/go/Alphasfile", b, iv, nil, TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	app := svcByName(af, "app")
	if app == nil || !app.Worktreeable() || app.UseOnly() {
		t.Fatalf("app must be a worktree service: %+v", app)
	}
	wantDir := "/repo/examples/go/.zordon/worktrees/main/src/app"
	if app.Runtime.Dir != wantDir {
		t.Errorf("app dir = %q, want %q", app.Runtime.Dir, wantDir)
	}
	if got := strings.Join(app.Runtime.Command, " "); !strings.Contains(got, "/bin/app -addr 127.0.0.1:") {
		t.Errorf("app cmd not resolved: %v", app.Runtime.Command)
	}

	// app_pinned / dlv are commented-out reference shapes (need network /
	// a real tag) so the example stays runnable offline.
	if svcByName(af, "app_pinned") != nil || svcByName(af, "dlv") != nil {
		t.Error("app_pinned/dlv expected commented out (offline-runnable example)")
	}
}
