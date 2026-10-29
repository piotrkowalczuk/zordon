package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// The ruby default build is a plain `bundle install`: the gem location
// travels as BUNDLE_PATH in the toolchain env, so nothing is written into
// the checkout — the old `bundle config set --local path` left a
// .bundle/config behind in every `dir` primary.
func TestDefaultBuild_ruby_noLocalConfigWrite(t *testing.T) {
	svc := &alphasfile.Service{
		Toolchain: alphasfile.ToolchainRuby,
		Runtime:   &alphasfile.RuntimeConfig{Name: "app", Dir: "/checkout"},
		Package:   &alphasfile.Package{Toolchain: alphasfile.ToolchainRuby, Src: "/checkout"},
	}
	got := defaultBuild(svc, "app", "/state/bin", "/checkout")
	if got != "bundle install" {
		t.Errorf("default build = %q, want plain \"bundle install\"", got)
	}
	if strings.Contains(got, "bundle config") {
		t.Errorf("default build still writes .bundle/config: %q", got)
	}
}

// BUNDLE_PATH rides in the toolchain tier: a service `env { BUNDLE_PATH }`
// (the user's explicit statement) still wins over zordon's default.
func TestServiceEnv_rubyBundlePathBelowServiceEnv(t *testing.T) {
	tc := map[string]string{"BUNDLE_PATH": "/state/bundle/app", "GEM_HOME": "/toolchain/gems"}

	got := envToMap(serviceEnv(nil, tc, nil, nil, nil, nil))
	if got["BUNDLE_PATH"] != "/state/bundle/app" {
		t.Errorf("BUNDLE_PATH = %q, want the toolchain default", got["BUNDLE_PATH"])
	}

	got = envToMap(serviceEnv(nil, tc, nil, nil, nil, map[string]string{"BUNDLE_PATH": "vendor/bundle"}))
	if got["BUNDLE_PATH"] != "vendor/bundle" {
		t.Errorf("BUNDLE_PATH = %q, want the service env override", got["BUNDLE_PATH"])
	}
	if got["GEM_HOME"] != "/toolchain/gems" {
		t.Errorf("GEM_HOME lost under the override: %v", got)
	}
}

// A checkout's own .bundle/config is reported for ruby services only, and
// only when present.
func TestLocalBundleConfig(t *testing.T) {
	dir := t.TempDir()
	ruby := &alphasfile.Service{Toolchain: alphasfile.ToolchainRuby}
	if _, ok := localBundleConfig(ruby, dir); ok {
		t.Error("reported a config that does not exist")
	}
	if err := zfs.EnsureDir(filepath.Join(dir, ".bundle")); err != nil {
		t.Fatal(err)
	}
	if err := zfs.AtomicWrite(filepath.Join(dir, ".bundle", "config"), []byte("---\nBUNDLE_PATH: \"vendor/bundle\"\n")); err != nil {
		t.Fatal(err)
	}
	path, ok := localBundleConfig(ruby, dir)
	if !ok || path != filepath.Join(dir, ".bundle", "config") {
		t.Errorf("localBundleConfig = %q, %v", path, ok)
	}
	if _, ok := localBundleConfig(&alphasfile.Service{Toolchain: alphasfile.ToolchainGo}, dir); ok {
		t.Error("non-ruby service reported a bundler config")
	}
}

func envToMap(kvs []string) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
