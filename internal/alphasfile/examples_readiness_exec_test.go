package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the exec readiness action: the example must resolve
// to a probe whose exec command is fully interpolated (no leftover ${...})
// and whose env overlay carries the picked port. Pure Compile — no spawn.
func TestExampleReadinessExecResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/readiness_exec/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.InvocationState{
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/readiness_exec/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/readiness_exec/Alphasfile", b, iv, nil, "", TestConfig{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	s := svcByName(af, "app")
	if s == nil || s.Runtime == nil || s.Runtime.Readiness == nil {
		t.Fatal("app readiness not resolved")
	}
	p := s.Runtime.Readiness
	if p.Exec == nil {
		t.Fatal("readiness exec action not resolved")
	}
	if p.HTTP != nil {
		t.Error("exec probe should not carry an http action")
	}
	if len(p.Exec.Command) == 0 {
		t.Fatal("exec command empty")
	}
	joined := strings.Join(p.Exec.Command, " ")
	if strings.Contains(joined, "${") {
		t.Errorf("exec command not interpolated: %q", joined)
	}
	port := p.Exec.Env["PORT"]
	if port == "" || strings.Contains(port, "${") {
		t.Errorf("exec env PORT not interpolated: %q", port)
	}
}
