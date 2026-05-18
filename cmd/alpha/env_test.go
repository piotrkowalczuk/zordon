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
	cmd, err := buildCmd(svc, "/tmp/x", nil)
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
