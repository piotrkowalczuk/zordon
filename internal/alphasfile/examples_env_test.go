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
		FsHash: "h0", TmpDir: "/tmp/zordon-h0",
		Worktree: invocation.MainWorktree,
		StateDir: "/repo/examples/env/.zordon/worktrees/main",
	}
	af, err := Compile("/repo/examples/env/Alphasfile", b, iv, nil, TestConfig{})
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

	want := []string{"/tmp/zordon-h0/app.env", "/tmp/zordon-h0/app.local.env"}
	if len(s.Runtime.Dotenv) != len(want) || s.Runtime.Dotenv[0] != want[0] || s.Runtime.Dotenv[1] != want[1] {
		t.Errorf("dotenv paths = %q, want %q (the generated file{}s, in order)", s.Runtime.Dotenv, want)
	}
	files := map[string]*File{}
	for _, f := range s.Runtime.Files {
		files[f.Name] = f
	}
	denv, denv2 := files["denv"], files["denv2"]
	if denv == nil || denv2 == nil {
		t.Fatal("file{} denv/denv2 not resolved")
	}
	if denv.Path != want[0] {
		t.Errorf("denv file path = %q, want %q", denv.Path, want[0])
	}
	if denv2.Path != want[1] {
		t.Errorf("denv2 file path = %q, want %q", denv2.Path, want[1])
	}
	if !strings.Contains(denv.Body, "DOTENV_FROM_FILE=1") || !strings.Contains(denv.Body, "OVERRIDE_ME=from-dotenv") {
		t.Errorf("denv body wrong:\n%s", denv.Body)
	}
	if !strings.Contains(denv2.Body, "DOTENV2_FROM_FILE=1") || !strings.Contains(denv2.Body, "DOTENV_ORDER=second") {
		t.Errorf("denv2 body wrong:\n%s", denv2.Body)
	}
}
