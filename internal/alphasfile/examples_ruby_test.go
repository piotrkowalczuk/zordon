package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the Ruby toolchain. Pure Compile — no bundler,
// no spawn. Ruby has no inferred binary: `cmd` is required and `build`
// is overridable (here a no-op so the example is offline-runnable).
func TestExampleRubyResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/ruby/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		Hash: "h0", TmpDir: "/tmp/zordon-h0",
		StateDir: "/repo/examples/ruby/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/ruby/Alphasfile", b, iv, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	app := svcByName(af, "app")
	if app == nil || !app.Worktreeable() || app.UseOnly() {
		t.Fatalf("app must be a worktree ruby service: %+v", app)
	}
	if app.Toolchain != ToolchainRuby {
		t.Errorf("toolchain = %q, want ruby", app.Toolchain)
	}
	if got := strings.Join(app.BuildCmd(), " "); got != "true" {
		t.Errorf("build cmd = %q, want \"true\"", got)
	}
	if got := strings.Join(app.Runtime.Command, " "); !strings.HasPrefix(got, "ruby examples/ruby/src/app/app.rb -addr 127.0.0.1:") {
		t.Errorf("app cmd not resolved: %v", app.Runtime.Command)
	}

	if svcByName(af, "sinatra") != nil {
		t.Error("sinatra expected commented out (offline-runnable example)")
	}
}
