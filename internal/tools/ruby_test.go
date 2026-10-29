package tools

import "testing"

// The gem world is pinned to the interpreter's own dir (no ~/.gem/ruby
// fallback), bundler's user home moves under the data dir per ruby
// version, and BUNDLER_VERSION follows the declared tool.
func TestRubyEnv_pinsGemWorldAndBundlerHome(t *testing.T) {
	const gemDir = "/z/toolchain/installs/ruby/3.3.6/lib/ruby/gems/3.3.0"
	env := rubyEnv(gemDir, "/z/toolchain", "3.3.6", map[string]string{"bundler": "2.5.6"})
	want := map[string]string{
		"GEM_HOME":                       gemDir,
		"GEM_PATH":                       gemDir,
		"BUNDLE_USER_HOME":               "/z/toolchain/ruby-home/3.3.6/bundle",
		"BUNDLER_VERSION":                "2.5.6",
		"BUNDLE_SILENCE_ROOT_WARNING":    "1",
		"BUNDLE_DISABLE_VERSION_CHECK":   "1",
		"BUNDLE_IGNORE_FUNDING_REQUESTS": "1",
	}
	assertEnv(t, env, want)
	for _, k := range []string{"BUNDLE_IGNORE_CONFIG", "BUNDLE_FROZEN", "BUNDLE_APP_CONFIG"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must stay unset (project .bundle/config is honored, frozen is the user's call)", k)
		}
	}
}

// Without a declared bundler the lockfile's BUNDLED WITH governs, so no
// BUNDLER_VERSION pin is emitted.
func TestRubyEnv_noBundlerVersionWhenUndeclared(t *testing.T) {
	env := rubyEnv("/gems", "/z/toolchain", "3.3.6", nil)
	if v, ok := env["BUNDLER_VERSION"]; ok {
		t.Errorf("BUNDLER_VERSION = %q, want unset", v)
	}
}
