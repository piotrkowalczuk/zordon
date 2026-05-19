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
	cmd, err := buildCmd(svc, "/tmp/x", nil, false, nil, nil)
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

// Closed-world sysenv whitelist: with no allow list, the spawned cmd's
// env contains NONE of the host shell's exports — even widely-present
// vars like HOME and PATH are filtered out. This is the chokepoint that
// stops user shell mise/asdf/rbenv pollution leaking into services.
func TestServiceEnv_emptyAllowDropsHostVars(t *testing.T) {
	t.Setenv("RUBYLIB", "/host/poison")
	t.Setenv("BUNDLE_GEMFILE", "/host/poison/Gemfile")
	t.Setenv("HOME", "/Users/test")

	got := serviceEnv(nil, nil, nil, nil)

	for _, kv := range got {
		if strings.HasPrefix(kv, "RUBYLIB=") || strings.HasPrefix(kv, "BUNDLE_") || strings.HasPrefix(kv, "HOME=") {
			t.Errorf("host var leaked through empty whitelist: %s", kv)
		}
	}
}

// With an explicit whitelist, only those keys cross over; everything
// else (RUBYLIB, BUNDLE_*, etc.) is stripped.
func TestServiceEnv_whitelistDropsEverythingElse(t *testing.T) {
	t.Setenv("HOME", "/Users/test")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("RUBYLIB", "/host/poison")
	t.Setenv("GEM_HOME", "/host/poison")
	t.Setenv("BUNDLE_GEMFILE", "/host/poison/Gemfile")

	got := serviceEnv(nil, nil, []string{"HOME", "LANG"}, nil)

	want := map[string]string{"HOME": "/Users/test", "LANG": "en_US.UTF-8"}
	gotMap := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		gotMap[k] = v
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("whitelisted %s missing or wrong: got %q, want %q", k, gotMap[k], v)
		}
	}
	for _, leaked := range []string{"RUBYLIB", "GEM_HOME", "BUNDLE_GEMFILE"} {
		if _, ok := gotMap[leaked]; ok {
			t.Errorf("non-whitelisted %s leaked through: %s", leaked, gotMap[leaked])
		}
	}
}

// Explicit env map overlays on top of the (filtered) host env.
func TestServiceEnv_explicitOverlay(t *testing.T) {
	t.Setenv("LANG", "C")
	got := serviceEnv(nil, map[string]string{"PATH": "/svc/bin", "EXTRA": "yes"}, []string{"LANG"}, nil)
	gotMap := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		gotMap[k] = v
	}
	if gotMap["LANG"] != "C" {
		t.Errorf("whitelisted LANG dropped: %v", gotMap)
	}
	if gotMap["PATH"] != "/svc/bin" || gotMap["EXTRA"] != "yes" {
		t.Errorf("explicit overlay missing: %v", gotMap)
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
