package tools

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
)

// probeRubyGemDir asks the pinned interpreter where its gems live
// (Gem.default_dir, e.g. installs/ruby/3.3.6/lib/ruby/gems/3.3.0). The
// ABI segment is not derivable from the version string for every ruby
// mise can install (truffleruby-24.1.0, 3.4.0-preview1), so the
// interpreter is the source of truth. Runs under the closed world;
// `mise exec` auto-installs the ruby when missing, so callers hold the
// per-(ruby, version) Acquire lock.
func probeRubyGemDir(binPath, dataDir, version string, host zenv.EnvironmentVariables, logOut io.Writer) (string, error) {
	spec := "ruby@" + version
	cmd := miseCommand(binPath, dataDir, host, "exec", spec, "--", "ruby", "-e", "print Gem.default_dir")
	cmd.Stderr = logOut
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("probe gem dir of %s (data dir %s): %w", spec, dataDir, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("probe gem dir of %s: ruby printed nothing", spec)
	}
	return dir, nil
}

// rubyEnv pins RubyGems and Bundler to the mise ruby and cuts every
// ambient source they would otherwise consult. mise itself sets nothing
// for ruby, and both tools lean on HOME:
//
//   - GEM_HOME / GEM_PATH = the interpreter's own gem dir. RubyGems
//     otherwise appends ~/.gem/ruby/<abi> to the search path whenever HOME
//     exists, so a host `gem install --user-install bundler` would shadow
//     the declared one. With the path pinned, the only bundler a service
//     can see is the one installed into this ruby.
//   - BUNDLE_USER_HOME: bundler's global config (~/.bundle/config —
//     credentials, path, mirrors), its compact-index cache and plugins move
//     under dataDir, per ruby version since plugins carry native code.
//   - BUNDLER_VERSION: when the Alphasfile declares `bundler` in tools,
//     the `bundle` shim runs exactly that version, and bundler's
//     lockfile auto-switch (which would `gem install` the lockfile's
//     BUNDLED WITH version over the network) is disabled. Undeclared, the
//     lockfile governs and any self-install lands in GEM_HOME.
//   - BUNDLE_SILENCE_ROOT_WARNING / BUNDLE_DISABLE_VERSION_CHECK /
//     BUNDLE_IGNORE_FUNDING_REQUESTS: log and network hygiene, the
//     bundler counterpart of npm's fund/audit knobs.
//
// Deliberately not set: BUNDLE_IGNORE_CONFIG (a checkout's own
// .bundle/config is project state and stays honored) and BUNDLE_FROZEN
// (the user's call via env). ~/.gemrc is skipped at install time via
// `gem install --norc`; bundler still reads it for proxy settings.
func rubyEnv(gemDir, dataDir, version string, tools map[string]string) zenv.EnvironmentVariables {
	env := zenv.EnvironmentVariables{
		"GEM_HOME":                       gemDir,
		"GEM_PATH":                       gemDir,
		"BUNDLE_USER_HOME":               filepath.Join(dataDir, "ruby-home", version, "bundle"),
		"BUNDLE_SILENCE_ROOT_WARNING":    "1",
		"BUNDLE_DISABLE_VERSION_CHECK":   "1",
		"BUNDLE_IGNORE_FUNDING_REQUESTS": "1",
	}
	if v := tools["bundler"]; v != "" {
		env["BUNDLER_VERSION"] = v
	}
	return env
}
