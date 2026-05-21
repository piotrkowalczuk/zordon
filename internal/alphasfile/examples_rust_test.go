package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the Rust toolchain. Pure Compile — no cargo, no
// spawn. `app` is the runnable worktree mode; the use-only / git+src
// reference shapes are commented out (offline-runnable example).
func TestExampleRustResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/rust/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/rust/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/rust/Alphasfile", b, iv, nil, TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	app := svcByName(af, "app")
	if app == nil || !app.Worktreeable() || app.UseOnly() {
		t.Fatalf("app must be a worktree rust service: %+v", app)
	}
	if app.Toolchain != ToolchainRust {
		t.Errorf("toolchain = %q, want rust", app.Toolchain)
	}
	wantDir := "/repo/examples/rust/.zordon/worktrees/main/src/app"
	if app.Runtime.Dir != wantDir {
		t.Errorf("app dir = %q, want %q", app.Runtime.Dir, wantDir)
	}
	if app.Package.Exe != "./examples/rust/src/app" {
		t.Errorf("app exe = %q", app.Package.Exe)
	}
	if got := strings.Join(app.Runtime.Command, " "); !strings.Contains(got, "/bin/app -addr 127.0.0.1:") {
		t.Errorf("app cmd not resolved: %v", app.Runtime.Command)
	}

	if svcByName(af, "broker") != nil || svcByName(af, "tansu") != nil {
		t.Error("broker/tansu expected commented out (offline-runnable example)")
	}
}
