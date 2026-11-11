// Package tools materializes language toolchains via mise without
// touching the user's mise installation.
//
// The contract:
//
//   - mise binary lives at <zordonHome>/bin/mise at the requested version
//     (DefaultMiseVersion unless overridden via --mise.version); if missing or
//     a different version, EnsureMise downloads and SHA-256-verifies the
//     release archive for the platform, falling back only to `cargo install
//     --locked --version <ver>`.
//   - tool installs live under <some>/.zordon/toolchain/installs/...,
//     where <some> is the nearest directory in the walk-up chain that
//     already has the requested version installed, or <zordonHome>
//     (== ~/.zordon) as the default install destination.
//   - per-spawn `mise env --json <tool>@<version>` is run with that
//     resolved data dir pinned via MISE_DATA_DIR. The user's mise
//     state (~/.local/share/mise/...) is never read or written.
//   - all install/env operations are serialized per-(tool, version) by
//     a file lock so concurrent services racing the same `gem install
//     bundler` (or mise install of the same Ruby) don't stomp each
//     other's tarball-extract.
//
// Hierarchy mirrors Alphasfile federation: a project-local override
// can shadow the global cache without rewriting it, which is what an
// agent updating a toolchain mid-flight wants.
package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// ResolveDataDir walks up from `from`, picking the nearest ancestor
// that already has `installs/<tool>/<version>` under its
// `.zordon/toolchain/`. That dir becomes MISE_DATA_DIR — installed
// versions there are reused, and any `mise install` would write
// there.
//
// Walk-up is BOUNDED by `filepath.Dir(defaultDataDir)` — i.e., we
// never walk above the directory that contains the default cache.
// Without this bound a custom test cache rooted inside a sandbox
// would walk all the way up and silently reuse the developer's
// real `~/.zordon/toolchain`, polluting it with test artifacts.
// In production (from == zordonHome, defaultDataDir == zordonHome+/toolchain)
// the bound is a no-op: the first iteration is already at it.
//
// If nothing in the chain has the version, returns `defaultDataDir`
// — the cache location chosen by the caller — so the next
// `mise install` populates it.
//
// `defaultDataDir` is what production wants as "<zordonHome>/toolchain"
// and what tests want as "<testCache>/toolchain"; we don't synthesize
// the path so the caller controls the convention.
func ResolveDataDir(from, defaultDataDir, tool, version string) string {
	tool = miseToolName(tool)
	bound := filepath.Dir(defaultDataDir)
	dir := from
	for {
		candidate := filepath.Join(dir, ".zordon", "toolchain")
		if zfs.Exists(filepath.Join(candidate, "installs", tool, version)) {
			return candidate
		}
		if dir == bound {
			return defaultDataDir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return defaultDataDir
		}
		dir = parent
	}
}

// IsInstalled reports whether tool@version is already materialized under dataDir.
func IsInstalled(dataDir, tool, version string) bool {
	return zfs.Exists(filepath.Join(dataDir, "installs", miseToolName(tool), version))
}

// EnsureToolchain materializes a language runtime version under dataDir via
// `mise install <lang>@<version>` when not yet installed, streaming progress to
// logOut. Callers hold the per-(lang, version) GlobalKeychain.LockTool lock.
func EnsureToolchain(binPath, dataDir, lang, version string, logOut io.Writer) error {
	if IsInstalled(dataDir, lang, version) {
		return nil
	}
	return EnsurePackage(binPath, dataDir, miseToolName(lang), version, nil, logOut)
}

// MiseEnv runs `mise env --json <tool>@<version>` with the resolved
// dataDir as MISE_DATA_DIR, returning the decoded env map. The user's
// mise state is never read or written — every MISE_*_DIR is forced to
// a zordon-owned location.
//
// JSON output is used because mise's shell-output modes (`-s bash` etc.)
// emit `export PATH=".../bin:$PATH"` with the shell expansion left
// literal — fine for sourcing, broken for setting cmd.Env. `--json`
// gives fully-resolved values so we just decode and overlay.
//
// Mise installs the version automatically if it's missing under
// dataDir; stderr is piped so progress shows on first install. Callers
// must hold the per-(tool, version) lock from GlobalKeychain.LockTool —
// auto-install races otherwise.
func MiseEnv(binPath, dataDir, tool, version string, logOut io.Writer) (map[string]string, error) {
	env, _, err := MiseEnvWithBinDirs(binPath, dataDir, tool, version, logOut)
	return env, err
}

// MiseEnvWithBinDirs is MiseEnv plus the bin directories mise prepends to
// PATH for this tool: the leading PATH entries the resolved env adds over the
// base env we hand mise (isolatedEnv). Those are exactly the dirs holding the
// tool's executables — e.g. postgres's bin with pg_dump/initdb — which alpha
// layers onto a *different* service's PATH so a provision can find a binary
// that ships with another service's package. Same locking contract as MiseEnv.
func MiseEnvWithBinDirs(binPath, dataDir, tool, version string, logOut io.Writer) (map[string]string, []string, error) {
	spec := miseToolName(tool) + "@" + version
	cmd := miseCommand(binPath, dataDir, "env", "--json", spec)
	base := cmd.Env
	cmd.Stderr = logOut
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("mise env %s (data dir %s): %w", spec, dataDir, err)
	}
	env := map[string]string{}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, nil, fmt.Errorf("decode mise env %s: %w", spec, err)
	}
	basePATH := ""
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			basePATH = v
			break
		}
	}
	dirs := zfs.NewResolver("", "").ForToolchain(env["PATH"], basePATH).ToolBinDirs()
	return env, dirs, nil
}

// EnsurePackage installs a `pkg` service's native package via mise
// (`mise install <name>@<version>`). name is the mise tool ref:
// backend-qualified (e.g. "aqua:etcd-io/etcd") or a bare name that
// mise's registry resolves to a backend (redis → vfox→asdf). Idempotent
// — mise no-ops when the version is already present under dataDir.
// Download/compile progress goes to logOut (some backends build from
// source on first install). Callers hold the per-(name, version)
// GlobalKeychain.LockTool lock, since the install writes under dataDir.
func EnsurePackage(binPath, dataDir, name, version string, buildEnv zenv.EnvironmentVariables, logOut io.Writer) error {
	spec := name + "@" + version
	cmd := miseCommand(binPath, dataDir, "install", spec)
	for k, v := range buildEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mise install %s (data dir %s): %w", spec, dataDir, err)
	}
	return nil
}

// EnsureTools installs the requested language-native tools into the
// pinned interpreter's tool world. Each version of a toolchain (Ruby
// 3.0.7 vs Ruby 3.3.10) has its own gem/pip/cargo registry, so these
// must be re-installed per pinned version.
//
// Runs through `mise exec <tool>@<version> -- <installer> ...` so each
// install lands in the right interpreter's world. A tool already present at
// its version is skipped: gem and npm reinstall it otherwise, rewriting
// files that a service started earlier may be loading at that moment.
//
// Returns the first install error or nil. Output (progress, warnings)
// goes to logOut so the user can see what's happening on first run.
func EnsureTools(binPath, dataDir, toolchain, version string, items map[string]string, logOut io.Writer) error {
	if len(items) == 0 {
		return nil
	}
	for name, ver := range items {
		if check := toolPresentCmd(binPath, dataDir, toolchain, version, name, ver); check != nil && check.Run() == nil {
			continue
		}
		cmd, err := toolInstallCmd(binPath, dataDir, toolchain, version, name, ver)
		if err != nil {
			return err
		}
		cmd.Stdout = logOut
		cmd.Stderr = logOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install %s tool %s@%s into %s@%s: %w", toolchain, name, ver, toolchain, version, err)
		}
	}
	return nil
}

// miseToolName maps the zordon-side language label to mise's tool
// name. Mostly identity; "nodejs" → "node" because mise installs node
// under installs/node/<ver> and `mise env --json node@<ver>` is the
// canonical spec.
func miseToolName(lang string) string {
	if lang == "nodejs" {
		return "node"
	}
	return lang
}

// isolatedEnv returns the env-var slice that pins mise to a zordon-
// owned location regardless of user's home mise installation. The
// mise binary's dir is also prepended to PATH so subprocesses that
// reach back to call `mise` resolve to the zordon-owned binary.
func isolatedEnv(dataDir, miseBin string) []string {
	cfgRoot := filepath.Dir(dataDir)
	var env []string
	for _, kv := range zenv.Environ() {
		if strings.HasPrefix(kv, "MISE_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"MISE_DATA_DIR="+dataDir,
		"MISE_CONFIG_DIR="+filepath.Join(cfgRoot, "mise-config"),
		"MISE_STATE_DIR="+filepath.Join(cfgRoot, "mise-state"),
		"MISE_CACHE_DIR="+filepath.Join(cfgRoot, "mise-cache"),
	)
	if miseBin == "" {
		return env
	}
	miseDir := filepath.Dir(miseBin)
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + miseDir + string(zfs.PathListSeparator) + kv[len("PATH="):]
			return env
		}
	}
	return append(env, "PATH="+miseDir)
}

func miseCommand(binPath, dataDir string, args ...string) *exec.Cmd {
	full := append([]string{"--no-config"}, args...)
	cmd := exec.Command(binPath, full...)
	cmd.Env = isolatedEnv(dataDir, binPath)
	return cmd
}

func toolInstallCmd(binPath, dataDir, toolchain, version, name, ver string) (*exec.Cmd, error) {
	spec := miseToolName(toolchain) + "@" + version
	var argv []string
	switch toolchain {
	case "ruby":
		argv = []string{"gem", "install", name, "--version", ver, "--no-document"}
	case "python":
		argv = []string{"pip", "install", name + "==" + ver}
	case "rust":
		argv = []string{"cargo", "install", name, "--version", ver, "--locked"}
	case "go":
		argv = []string{"go", "install", name + "@" + ver}
	case "nodejs":
		argv = []string{"npm", "install", "-g", "--no-fund", "--no-audit", name + "@" + ver}
	case "java":
		return nil, fmt.Errorf("toolchain java: no tool installer — Maven/Gradle come from the project wrapper (./mvnw / ./gradlew), not toolchain.java.tools")
	default:
		return nil, fmt.Errorf("toolchain %q: no tool installer wired", toolchain)
	}
	full := append([]string{"exec", spec, "--"}, argv...)
	return miseCommand(binPath, dataDir, full...), nil
}

func toolPresentCmd(binPath, dataDir, toolchain, version, name, ver string) *exec.Cmd {
	var argv []string
	switch toolchain {
	case "ruby":
		argv = []string{"gem", "list", "--installed", "--exact", name, "--version", ver}
	case "nodejs":
		argv = []string{"npm", "ls", "--global", "--depth=0", name + "@" + ver}
	default:
		return nil
	}
	full := append([]string{"exec", miseToolName(toolchain) + "@" + version, "--"}, argv...)
	return miseCommand(binPath, dataDir, full...)
}
