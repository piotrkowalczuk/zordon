package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
)

// Pinpoints TODO "ENV nie jest wstrzykiwane do procesu": does buildCmd put
// the resolved env { } map onto exec.Cmd.Env?
func TestBuildCmdInjectsEnv(t *testing.T) {
	svc := &alphasfile.Service{
		Toolchain: alphasfile.ToolchainGo,
		Runtime: &alphasfile.RuntimeConfig{
			Name:    "app",
			Command: []string{"/bin/echo", "hi"},
			Env:     map[string]string{"ZTEST_INJECT": "yes"},
		},
		Package: &alphasfile.Package{Toolchain: alphasfile.ToolchainGo, Src: "/tmp/x"},
	}
	cmd, err := buildCmd(svc, "/tmp/x", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Env == nil {
		t.Fatal("cmd.Env is nil — env map not injected at all")
	}
	if !slices.Contains(cmd.Env, "ZTEST_INJECT=yes") {
		var got []string
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "ZTEST_") {
				got = append(got, kv)
			}
		}
		t.Fatalf("ZTEST_INJECT=yes missing from cmd.Env (ZTEST_* = %v)", got)
	}
}

// Phase env: runtime{} overlays the base env{}; agent{} overlays on top
// only when alpha runs in --agent mode. Build env stays separate.
func TestPhaseEnvPrecedence(t *testing.T) {
	svc := &alphasfile.Service{
		Toolchain: alphasfile.ToolchainGo,
		Runtime: &alphasfile.RuntimeConfig{
			Name:     "app",
			Env:      map[string]string{"LEVEL": "base", "KEEP": "1"},
			RunEnv:   map[string]string{"LEVEL": "runtime"},
			BuildEnv: map[string]string{"LEVEL": "build"},
			AgentEnv: map[string]string{"LEVEL": "agent", "QUIET": "1"},
		},
	}
	// runtime, no agent: runtime overrides base, agent NOT applied.
	got := phaseEnv(svc, svc.Runtime.RunEnv, false)
	if got["LEVEL"] != "runtime" || got["KEEP"] != "1" || got["QUIET"] != "" {
		t.Fatalf("runtime/no-agent wrong: %v", got)
	}
	// runtime, agent on: agent overlay wins and adds its keys.
	got = phaseEnv(svc, svc.Runtime.RunEnv, true)
	if got["LEVEL"] != "agent" || got["QUIET"] != "1" {
		t.Fatalf("runtime/agent wrong: %v", got)
	}
	// build phase is independent of runtime.
	got = phaseEnv(svc, svc.Runtime.BuildEnv, false)
	if got["LEVEL"] != "build" {
		t.Fatalf("build wrong: %v", got)
	}
	got = phaseEnv(svc, svc.Runtime.BuildEnv, true)
	if got["LEVEL"] != "agent" {
		t.Fatalf("build/agent overlay wrong: %v", got)
	}
}
