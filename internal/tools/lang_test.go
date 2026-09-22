package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Go's ambient config goes dark: the user env file (GOENV=off), ~/.netrc
// (NETRC relocated) and the build cache all leave HOME; GOTOOLCHAIN stays
// pinned to the mise-installed version.
func TestLangEnv_go_relocatesAmbient(t *testing.T) {
	env, err := LangEnv("go", "", "/z/toolchain", "1.26.2", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GOTOOLCHAIN": "local",
		"GOENV":       "off",
		"NETRC":       "/z/toolchain/go-home/netrc",
		"GOCACHE":     "/z/toolchain/go-home/build-cache",
	}
	assertEnv(t, env, want)
}

// Every node package manager's user config and cache is relocated under
// the data dir, alongside the existing banner/prompt knobs.
func TestLangEnv_node_relocatesAmbient(t *testing.T) {
	env, err := LangEnv("nodejs", "", "/z/toolchain", "22.11.0", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"NPM_CONFIG_FUND":                 "false",
		"NPM_CONFIG_AUDIT":                "false",
		"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
		"NPM_CONFIG_USERCONFIG":           "/z/toolchain/node-home/npmrc",
		"NPM_CONFIG_CACHE":                "/z/toolchain/node-home/npm-cache",
		"COREPACK_HOME":                   "/z/toolchain/node-home/corepack",
		"YARN_CACHE_FOLDER":               "/z/toolchain/node-home/yarn-cache",
		"npm_config_store_dir":            "/z/toolchain/node-home/pnpm-store",
		"BUN_INSTALL_CACHE_DIR":           "/z/toolchain/node-home/bun-cache",
	}
	assertEnv(t, env, want)
}

// Maven and Gradle homes move under the data dir; the user settings.xml
// MAVEN_ARGS points at is materialized so Maven doesn't abort on `-s`.
func TestLangEnv_java_relocatesAmbient(t *testing.T) {
	dataDir := t.TempDir()
	env, err := LangEnv("java", "", dataDir, "temurin-21", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dataDir, "java-home", "m2", "settings.xml")
	want := map[string]string{
		"GRADLE_USER_HOME": filepath.Join(dataDir, "java-home", "gradle"),
		"MAVEN_USER_HOME":  filepath.Join(dataDir, "java-home", "m2"),
		"MAVEN_SKIP_RC":    "1",
		"MAVEN_ARGS":       "-s " + settings + " -Dmaven.repo.local=" + filepath.Join(dataDir, "java-home", "m2", "repository"),
	}
	assertEnv(t, env, want)
	if !zfs.Exists(settings) {
		t.Fatalf("settings.xml not written at %s", settings)
	}
	body, err := zfs.Read(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<settings") {
		t.Errorf("settings.xml is not a settings document: %s", body)
	}
}

// Languages without layer-3 knowledge (pkg refs, unknown labels) yield
// nothing rather than an error — the toolchain simply runs on mise's env.
func TestLangEnv_unknownLangIsEmpty(t *testing.T) {
	env, err := LangEnv("aqua:etcd-io/etcd", "", "/z/toolchain", "3.5.17", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 0 {
		t.Errorf("unexpected env for a pkg ref: %v", env)
	}
}

// Only ruby has per-service layer-3 state: its bundle path.
func TestServiceEnv_rubyBundlePath(t *testing.T) {
	if got := ServiceEnv("ruby", "/state/bundle/app"); got["BUNDLE_PATH"] != "/state/bundle/app" {
		t.Errorf("ruby ServiceEnv = %v, want BUNDLE_PATH", got)
	}
	if got := ServiceEnv("go", "/state/bundle/app"); got != nil {
		t.Errorf("go ServiceEnv = %v, want nil", got)
	}
	if got := ServiceEnv("ruby", ""); got != nil {
		t.Errorf("ruby ServiceEnv without a bundle dir = %v, want nil", got)
	}
}

func assertEnv(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
