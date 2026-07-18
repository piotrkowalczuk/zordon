package tools

import (
	"strings"
	"testing"
)

// Every mise subcommand must carry the leading --no-config global flag so a
// stray mise.toml (next to the Alphasfile or any ancestor), the global
// config, or ~/.tool-versions can't pollute the CLI-pinned tool@version.
func TestMiseCommand_prependsNoConfig(t *testing.T) {
	cmd := miseCommand("/z/bin/mise", "/z/toolchain", "install", "go@1.26.2")
	want := []string{"/z/bin/mise", "--no-config", "install", "go@1.26.2"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

// isolatedEnv drops every host MISE_* (so a developer's MISE_ENV can't leak)
// and sets each MISE_*_DIR exactly once at a zordon-owned location (no
// earlier host duplicate surviving to win).
func TestIsolatedEnv_stripsHostMise(t *testing.T) {
	t.Setenv("MISE_ENV", "dev")
	t.Setenv("MISE_DATA_DIR", "/host/data")

	const dataDir = "/z/toolchain"
	env := isolatedEnv(dataDir, "")

	var dataDirs []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "MISE_ENV=") {
			t.Errorf("host MISE_ENV leaked into isolated env: %q", kv)
		}
		if v, ok := strings.CutPrefix(kv, "MISE_DATA_DIR="); ok {
			dataDirs = append(dataDirs, v)
		}
	}
	if len(dataDirs) != 1 || dataDirs[0] != dataDir {
		t.Errorf("MISE_DATA_DIR = %v, want exactly [%q] (no host duplicate)", dataDirs, dataDir)
	}
}
