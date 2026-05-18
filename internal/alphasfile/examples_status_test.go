package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the per-service `print` line: it must resolve as
// an interpolated sink (sees self.vars). Pure Compile — no spawn.
func TestExampleStatusResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/status/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		Hash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/status/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/status/Alphasfile", b, iv, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	s := svcByName(af, "app")
	if s == nil || s.Runtime == nil {
		t.Fatal("app not resolved")
	}
	p := s.Runtime.Print
	if !strings.HasPrefix(p, "http://127.0.0.1:") || strings.Contains(p, "${") {
		t.Fatalf("print not interpolated: %q", p)
	}
	if !strings.HasSuffix(p, "(app endpoint)") {
		t.Errorf("print literal tail lost: %q", p)
	}
}
