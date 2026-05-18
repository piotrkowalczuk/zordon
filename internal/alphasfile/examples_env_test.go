package alphasfile

import (
	"os"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
)

// Regression oracle for examples/env: env{} + dotenv both resolve, env{}
// overrides dotenv, dynamic values interpolate, and the dotenv path is
// the generated file{} path. Pure Compile — no spawn.
func TestExampleEnvResolves(t *testing.T) {
	b, err := os.ReadFile("../../examples/env/Alphasfile")
	if err != nil {
		t.Fatal(err)
	}
	iv := &invocation.Invocation{
		Hash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/env/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/env/Alphasfile", b, iv, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	s := svcByName(af, "app")
	if s == nil || s.Runtime == nil {
		t.Fatal("app not resolved")
	}

	// src-only in the main worktree → in place: build/run from src as-is
	// (no git worktree add, no HEAD reset → uncommitted edits work). dir
	// is the resolved src (../.. of the Alphasfile), NOT a CheckoutPath.
	if !s.Package.InPlace {
		t.Error("src-only @ main must be InPlace")
	}
	if s.Runtime.Dir != "/repo" {
		t.Errorf("in-place dir = %q, want /repo (resolved ../..)", s.Runtime.Dir)
	}
	if strings.Contains(s.Runtime.Dir, "/.zordon/worktrees/") {
		t.Errorf("in-place must not be a worktree checkout: %q", s.Runtime.Dir)
	}

	if s.Runtime.Env["ENV_STATIC"] != "hello" {
		t.Errorf("ENV_STATIC = %q", s.Runtime.Env["ENV_STATIC"])
	}
	if dyn := s.Runtime.Env["ENV_DYN"]; !strings.HasPrefix(dyn, "127.0.0.1:") || dyn == "127.0.0.1:" {
		t.Errorf("ENV_DYN not interpolated: %q", dyn)
	}
	if s.Runtime.Env["OVERRIDE_ME"] != "from-env" {
		t.Errorf("env{} must win over dotenv: OVERRIDE_ME=%q", s.Runtime.Env["OVERRIDE_ME"])
	}

	want := "/tmp/zordon-h0/app.env"
	if s.Runtime.Dotenv != want {
		t.Errorf("dotenv path = %q, want %q (the generated file{})", s.Runtime.Dotenv, want)
	}
	var denv *File
	for _, f := range s.Runtime.Files {
		if f.Name == "denv" {
			denv = f
		}
	}
	if denv == nil {
		t.Fatal("file{} denv not resolved")
	}
	if denv.Path != want {
		t.Errorf("denv file path = %q, want %q", denv.Path, want)
	}
	if !strings.Contains(denv.Body, "DOTENV_FROM_FILE=1") || !strings.Contains(denv.Body, "OVERRIDE_ME=from-dotenv") {
		t.Errorf("denv body wrong:\n%s", denv.Body)
	}
}
