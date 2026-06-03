package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Oracle for the build/runtime/agent phase blocks: each block's `env`
// resolves into its own map (interpolated, in the DAG), the build
// command moves into build{command}, and the phases stay isolated
// (build vars never leak into runtime). Pure Compile — no spawn.
func TestExamplePhasesResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/phases/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/phases/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/phases/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	s := svcByName(af, "app")
	if s == nil || s.Runtime == nil {
		t.Fatal("app not resolved")
	}
	rt := s.Runtime

	if got := rt.BuildEnv["BUILD_TAG"]; got != "compiled-with-build-env" {
		t.Errorf("build.env BUILD_TAG = %q", got)
	}
	if got := rt.RunEnv["VERBOSITY"]; got != "loud" {
		t.Errorf("runtime.env VERBOSITY = %q", got)
	}
	if got := rt.RunEnv["RUNTIME_ONLY"]; got != "runtime-value" {
		t.Errorf("runtime.env RUNTIME_ONLY = %q", got)
	}
	if got := rt.AgentEnv["VERBOSITY"]; got != "quiet" {
		t.Errorf("agent.env VERBOSITY = %q", got)
	}
	// Phase isolation: a build-only var must not appear in runtime env.
	if _, leaked := rt.RunEnv["BUILD_TAG"]; leaked {
		t.Error("BUILD_TAG leaked into runtime env")
	}
	if len(rt.Env) != 0 {
		t.Errorf("no top-level env expected, got %v", rt.Env)
	}
	// build.cmd is argv: ["sh","-lc","<script>"]. ${fs::bin()} is
	// interpolated by zordon; the shell var $BUILD_TAG survives for the
	// nested shell (it reads it from build.env at exec time).
	bc := s.BuildCmd()
	if len(bc) != 3 || bc[0] != "sh" || bc[1] != "-lc" {
		t.Fatalf("build.cmd should be [sh -lc <script>], got %v", bc)
	}
	script := bc[2]
	if !strings.HasPrefix(script, "go build -ldflags") || strings.Contains(script, "${") {
		t.Fatalf("build script not resolved: %q", script)
	}
	if !strings.Contains(script, "main.BuiltBy=$BUILD_TAG") {
		t.Errorf("shell var $BUILD_TAG should survive interpolation: %q", script)
	}
}
