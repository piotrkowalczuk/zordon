package tools

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
)

// Every mise subcommand must carry the leading --no-config global flag so a
// stray mise.toml (next to the Alphasfile or any ancestor), the global
// config, or ~/.tool-versions can't pollute the CLI-pinned tool@version.
func TestMiseCommand_prependsNoConfig(t *testing.T) {
	cmd := miseCommand("/z/bin/mise", "/z/toolchain", nil, "install", "go@1.26.2")
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
	env := isolatedEnv(zenv.FromHost([]string{"MISE_ENV", "MISE_DATA_DIR"}), dataDir, "")

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

// Tool installs run under the sysenv closed world: a host GEM_HOME /
// BUNDLE_* / NPM_CONFIG_* / GOFLAGS / CARGO_HOME that the Alphasfile did
// not declare never reaches `mise exec`, so it cannot steer where `gem
// install bundler` or `cargo install` land.
func TestIsolatedEnv_dropsHostToolVars(t *testing.T) {
	poison := map[string]string{
		"GEM_HOME":            "/poison/gems",
		"BUNDLE_GEMFILE":      "/poison/Gemfile",
		"NPM_CONFIG_REGISTRY": "http://127.0.0.1:1",
		"GOFLAGS":             "-mod=vendor",
		"CARGO_HOME":          "/poison/cargo",
	}
	for k, v := range poison {
		t.Setenv(k, v)
	}
	t.Setenv("HOME", "/home/dev")

	got := envMap(isolatedEnv(zenv.FromHost([]string{"HOME", "PATH"}), "/z/toolchain", ""))

	for k := range poison {
		if v, ok := got[k]; ok {
			t.Errorf("undeclared host %s=%q leaked into the install env", k, v)
		}
	}
	if got["HOME"] != "/home/dev" {
		t.Errorf("declared HOME dropped: %v", got)
	}
}

// Declared sysenv keys still reach the install — the closed world is the
// Alphasfile's, not an empty one.
func TestIsolatedEnv_keepsAllowedHostVars(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
	t.Setenv("SSL_CERT_FILE", "/etc/corp.pem")

	got := envMap(isolatedEnv(zenv.FromHost([]string{"HTTPS_PROXY", "SSL_CERT_FILE"}), "/z/toolchain", ""))

	if got["HTTPS_PROXY"] != "http://proxy.corp:3128" || got["SSL_CERT_FILE"] != "/etc/corp.pem" {
		t.Errorf("declared host vars missing: %v", got)
	}
}

// When the Alphasfile does not declare PATH, mise still has to find
// git/curl/tar, so the host PATH is used — with the zordon mise bin dir
// first so `mise reshim` hooks resolve to the zordon-owned binary.
func TestIsolatedEnv_pathFallsBackToHostWhenUndeclared(t *testing.T) {
	t.Setenv("PATH", "/host/bin:/usr/bin")

	got := envMap(isolatedEnv(zenv.FromHost(nil), "/z/toolchain", "/z/bin/mise"))

	want := "/z/bin" + string(filepath.ListSeparator) + "/host/bin:/usr/bin"
	if got["PATH"] != want {
		t.Errorf("PATH = %q, want %q", got["PATH"], want)
	}
}

// A declared PATH is used as-is (plus the mise bin dir), not the host one.
func TestIsolatedEnv_declaredPathWins(t *testing.T) {
	t.Setenv("PATH", "/declared/bin")

	got := envMap(isolatedEnv(zenv.FromHost([]string{"PATH"}), "/z/toolchain", "/z/bin/mise"))

	if !strings.HasPrefix(got["PATH"], "/z/bin"+string(filepath.ListSeparator)+"/declared/bin") {
		t.Errorf("PATH = %q, want mise bin then the declared PATH", got["PATH"])
	}
}

// mise's rust backend installs rustup into $MISE_RUSTUP_HOME and cargo
// tools into $MISE_CARGO_HOME/bin, defaulting to the user's ~/.rustup and
// ~/.cargo. Both are pinned under the data dir so zordon never touches the
// developer's rust state and a host CARGO_HOME can't redirect installs.
func TestIsolatedEnv_pinsRustHomes(t *testing.T) {
	t.Setenv("MISE_CARGO_HOME", "/host/cargo")

	got := envMap(isolatedEnv(zenv.FromHost([]string{"MISE_CARGO_HOME"}), "/z/toolchain", ""))

	if got["MISE_CARGO_HOME"] != "/z/toolchain/cargo-home" {
		t.Errorf("MISE_CARGO_HOME = %q, want /z/toolchain/cargo-home", got["MISE_CARGO_HOME"])
	}
	if got["MISE_RUSTUP_HOME"] != "/z/toolchain/rustup-home" {
		t.Errorf("MISE_RUSTUP_HOME = %q, want /z/toolchain/rustup-home", got["MISE_RUSTUP_HOME"])
	}
}

// `gem install` skips ~/.gemrc: a `gem: --user-install` line there would
// divert the declared bundler into ~/.gem/ruby/<abi>, outside the pinned
// ruby, and the runtime GEM_PATH pin would then never see it.
func TestToolInstallCmd_ruby_skipsGemrc(t *testing.T) {
	cmd, err := toolInstallCmd("/z/bin/mise", "/z/toolchain", "ruby", "3.3.6", "bundler", "2.5.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Args, "--norc") {
		t.Errorf("gem install argv lacks --norc: %v", cmd.Args)
	}
	if !slices.Contains(cmd.Args, "--no-document") {
		t.Errorf("gem install argv lacks --no-document: %v", cmd.Args)
	}
}

func envMap(kvs []string) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
