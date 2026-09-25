package tools

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
)

// LangEnv returns the language's layer-3 environment: the pins and
// relocations zordon adds on top of mise's `env --json` output and BELOW
// the user's toolchain.<lang>.env overlay. Layered model:
//
//  1. sysenv whitelist  — filters host env (services AND installs)
//  2. mise env --json   — toolchain base (PATH, GOROOT, JAVA_HOME, ...)
//  3. LangEnv           — this function
//  4. toolchain.<lang>.env  — user override (power-user path, no safety net)
//
// Layer 3 has two jobs per language. First, make sure mise's pin actually
// takes effect at the language-runtime level and silence interactive
// prompts that would deadlock under alpha's piped stdin (Go's GOTOOLCHAIN,
// Corepack's download prompt, npm's banners). Second, relocate every
// HOME-based config file and cache the language's tooling would otherwise
// read or write — ~/.config/go/env, ~/.npmrc, ~/.m2, ~/.gradle, ~/.bundle,
// ~/.gem — to a zordon-owned directory under dataDir. sysenv passes HOME
// through (the tools need one), so without this the build of a service
// depends on whatever the developer's home happens to contain; with it,
// the only way machine-local state reaches a service is a declared
// Alphasfile channel (env, dotenv, sysenv).
//
// Callers apply the result with zenv.Fill so a value mise already pinned
// wins, and hand it to the tool installers as part of their closed world
// so `gem install` / `go install` / `npm -g` run under the same pins as
// the service later does. Ruby needs a probe of the pinned interpreter and
// Java a settings file on disk, so both may run a subprocess or write
// under dataDir; the other languages are pure.
func LangEnv(lang, binPath, dataDir, version string, tools map[string]string, host zenv.EnvironmentVariables, logOut io.Writer) (zenv.EnvironmentVariables, error) {
	switch lang {
	case "go":
		return goEnv(dataDir), nil
	case "nodejs":
		return nodeEnv(dataDir), nil
	case "java":
		return javaEnv(dataDir)
	case "ruby":
		gemDir, err := probeRubyGemDir(binPath, dataDir, version, host, logOut)
		if err != nil {
			return nil, err
		}
		return rubyEnv(gemDir, dataDir, version, tools), nil
	}
	return nil, nil
}

// ServiceEnv is the per-service slice of layer 3: values that depend on
// the service rather than the toolchain. Only Ruby has one today — its
// bundle lives out of tree at bundleDir, keyed by service because every
// service resolves its own Gemfile.
func ServiceEnv(lang, bundleDir string) zenv.EnvironmentVariables {
	if lang != "ruby" || bundleDir == "" {
		return nil
	}
	return zenv.EnvironmentVariables{"BUNDLE_PATH": bundleDir}
}

// goEnv pins the Go runtime to the mise-installed version and cuts the
// ambient config Go would otherwise read:
//
//   - GOTOOLCHAIN=local: Go's default `auto` reads each project's go.mod /
//     go.work `toolchain` directive and auto-downloads whatever version it
//     requests — defeating the zordon-Alphasfile pin. With `local`, Go uses
//     the version it was invoked as and fails loudly if a go.mod requires
//     more. A power user's toolchain.go.env can still say `auto`.
//   - GOENV=off: `go env -w` writes GOPROXY / GOFLAGS / GOPRIVATE / ... into
//     ~/.config/go/env (or the platform config dir), which Go then applies
//     to every build. off is Go's documented switch for ignoring that file.
//   - NETRC: Go reads ~/.netrc for module-proxy credentials; pointing it at
//     a zordon-owned path makes credentials a declared channel.
//   - GOCACHE: the build cache moves out of the user's cache dir.
//
// GOPATH / GOBIN / GOROOT are already per-version under the mise install.
// Go's telemetry counters still land in the user config dir: the location
// is not settable through the environment (GOTELEMETRYDIR is read-only).
func goEnv(dataDir string) zenv.EnvironmentVariables {
	home := filepath.Join(dataDir, "go-home")
	return zenv.EnvironmentVariables{
		"GOTOOLCHAIN": "local",
		"GOENV":       "off",
		"NETRC":       filepath.Join(home, "netrc"),
		"GOCACHE":     filepath.Join(home, "build-cache"),
	}
}

// nodeEnv silences npm's per-install banners and Corepack's download
// prompt, and relocates every package manager's user config and cache.
// The PATH side of the pin is handled by `mise env --json node@<ver>`
// (node bin dir) and EnsureNodeCorepack (pnpm/yarn shim dir on top).
//
//   - NPM_CONFIG_FUND / NPM_CONFIG_AUDIT = false: drop npm's "consider
//     funding ..." / vulnerability-count banner on every install.
//   - COREPACK_ENABLE_DOWNLOAD_PROMPT=0: Corepack's "About to download X,
//     [Y/n]?" is fine interactively, deadlocks under alpha's piped stdin.
//   - NPM_CONFIG_USERCONFIG: ~/.npmrc (registry, auth tokens, scopes) is
//     replaced by a zordon-owned file. npm and pnpm both tolerate a missing
//     one, which is the default state of a fresh HOME anyway.
//   - NPM_CONFIG_CACHE, COREPACK_HOME, YARN_CACHE_FOLDER (yarn 1),
//     npm_config_store_dir (pnpm), BUN_INSTALL_CACHE_DIR: caches move out of
//     ~/.npm, ~/.cache/node/corepack, ~/.yarn, ~/.local/share/pnpm, ~/.bun.
//
// Yarn berry's ~/.yarnrc.yml and bun's ~/.bunfig.toml have no env override
// upstream and stay HOME-bound.
func nodeEnv(dataDir string) zenv.EnvironmentVariables {
	home := filepath.Join(dataDir, "node-home")
	return zenv.EnvironmentVariables{
		"NPM_CONFIG_FUND":                 "false",
		"NPM_CONFIG_AUDIT":                "false",
		"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
		"NPM_CONFIG_USERCONFIG":           filepath.Join(home, "npmrc"),
		"NPM_CONFIG_CACHE":                filepath.Join(home, "npm-cache"),
		"COREPACK_HOME":                   filepath.Join(home, "corepack"),
		"YARN_CACHE_FOLDER":               filepath.Join(home, "yarn-cache"),
		"npm_config_store_dir":            filepath.Join(home, "pnpm-store"),
		"BUN_INSTALL_CACHE_DIR":           filepath.Join(home, "bun-cache"),
	}
}

// javaEnv relocates the Maven and Gradle homes. mise pins JAVA_HOME only;
// the build tools come from the project's wrapper, and both wrappers and
// tools default their config, repository and distribution caches to HOME:
//
//   - GRADLE_USER_HOME: ~/.gradle (gradle.properties, init scripts, caches).
//   - MAVEN_USER_HOME: where the wrapper unpacks its Maven distribution
//     (default ~/.m2/wrapper).
//   - MAVEN_ARGS (Maven >= 3.9): `-s` points user settings.xml (mirrors,
//     servers, proxies) at a zordon-owned file and maven.repo.local moves the
//     artifact repository out of ~/.m2/repository. Maven refuses a missing
//     `-s` file, so the settings file is written on first use.
//   - MAVEN_SKIP_RC=1: the mvn launcher otherwise sources ~/.mavenrc.
//
// MAVEN_ARGS is whitespace-split by Maven's launcher; a dataDir containing
// spaces would break it. ~/.zordon does not, and the fix (quoting) is not
// portable across launcher versions, so the limitation is accepted.
func javaEnv(dataDir string) (zenv.EnvironmentVariables, error) {
	home := filepath.Join(dataDir, "java-home")
	m2 := filepath.Join(home, "m2")
	settings := filepath.Join(m2, "settings.xml")
	if err := ensureMavenSettings(settings); err != nil {
		return nil, err
	}
	return zenv.EnvironmentVariables{
		"GRADLE_USER_HOME": filepath.Join(home, "gradle"),
		"MAVEN_USER_HOME":  m2,
		"MAVEN_SKIP_RC":    "1",
		"MAVEN_ARGS":       fmt.Sprintf("-s %s -Dmaven.repo.local=%s", settings, filepath.Join(m2, "repository")),
	}, nil
}
