package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for the federation project's `print`: it must
// surface the per-worktree domain (prometheus.<pathhash>.test) on the
// Caddy port pulled from the federation parent context. Pure Compile.
func TestExampleFederationPrint(t *testing.T) {
	b, err := os.ReadFile("../../examples/federation/project/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		Hash: "feedface00001111", TmpDir: "/tmp/zordon-feedface00001111",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/federation/project/.zordon/worktrees/main",
	}
	// Federation parent: caddy resolved with its vars (http + config_dir),
	// the project reads service.go.caddy.vars.* through the flat namespace.
	parent := NewParentContext([]*Service{{
		Toolchain: ToolchainGo,
		Runtime: &RuntimeConfig{
			Name: "caddy",
			Vars: map[string]any{"http": int64(8080), "config_dir": "/tmp/conf.d"},
		},
		Package: &Package{Toolchain: ToolchainGo, Git: "github.com/caddyserver/caddy"},
	}})

	af, err := Compile("/repo/examples/federation/project/Alphasfile", b, iv, parent)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	s := svcByName(af, "prometheus")
	if s == nil || s.Runtime == nil {
		t.Fatal("prometheus not resolved")
	}
	p := s.Runtime.Print
	wantHost := "prometheus.feedface00001111.test"
	if !strings.Contains(p, "http://"+wantHost+":8080/") {
		t.Errorf("print missing domain+hash+caddy port: %q", p)
	}
	if !strings.Contains(p, "worktree feedface00001111") {
		t.Errorf("print missing worktree hash tail: %q", p)
	}
	if strings.Contains(p, "${") {
		t.Errorf("print not fully interpolated: %q", p)
	}
}
