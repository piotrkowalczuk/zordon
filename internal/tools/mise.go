// Package tools materializes language toolchains via mise without
// touching the user's mise installation.
//
// The contract:
//
//   - mise binary lives at <zordonHome>/bin/mise; if missing, we
//     bootstrap via `cargo install` (rust is already required for any
//     zordon-managed crate service, so cargo's reasonable to require).
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

// EnsureMise returns the path to the zordon-owned mise binary at
// <zordonHome>/bin/mise. If it isn't installed yet, it bootstraps via
// `cargo install --root <zordonHome> --locked mise`. Subsequent calls
// are no-ops once the binary exists.
//
// stderr from cargo install is piped to logOut so the user sees
// progress on a slow first run.
func EnsureMise(zordonHome string, logOut io.Writer) (string, error) {
	bin := filepath.Join(zordonHome, "bin", "mise")
	if zfs.Exists(bin) {
		return bin, nil
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		return "", fmt.Errorf("zordon needs mise to materialize declared toolchains; "+
			"install cargo so zordon can `cargo install` it to %s, "+
			"or pre-place a mise binary there yourself", bin)
	}
	if err := zfs.EnsureDir(filepath.Join(zordonHome, "bin")); err != nil {
		return "", err
	}
	// Serialize cargo install so two alphas starting at the same time
	// don't try to write the same binary. Re-check existence under the
	// lock — the other waiter just did it for us.
	release, err := zfs.AcquireLock(filepath.Join(zordonHome, "locks"), "mise")
	if err != nil {
		return "", err
	}
	defer release()
	if zfs.Exists(bin) {
		return bin, nil
	}
	cmd := exec.Command("cargo", "install", "--root", zordonHome, "--locked", "mise")
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cargo install mise: %w", err)
	}
	return bin, nil
}

// miseToolName maps the zordon-side language label to mise's tool
// name. Mostly identity; "nodejs" → "node" because mise installs node
// under installs/node/<ver> and `mise env --json node@<ver>` is the
// canonical spec. (Mise accepts "nodejs@<ver>" as an alias, but the
// on-disk path is "node", which the walk-up cache lookup must match.)
func miseToolName(lang string) string {
	if lang == "nodejs" {
		return "node"
	}
	return lang
}

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

// isolatedEnv returns the env-var slice that pins mise to a zordon-
// owned location regardless of user's home mise installation. The
// mise binary's dir is also prepended to PATH so subprocesses that
// reach back to call `mise` resolve to the zordon-owned binary.
//
// The recursive-mise case is real: when mise installs node, it
// replaces node's `npm` script with a wrapper that runs `mise reshim`
// post-install. Without mise on PATH, that hook fails with exit 127
// ("mise: command not found"). Whether the same wrapping happens for
// other toolchains (ruby's gem, python's pip) is mise-internal and
// version-dependent, so this is unconditional rather than gated per
// language — the PATH addition is harmless when nothing in the
// subprocess tree ever invokes mise.
//
// alpha's host PATH may not have ~/.zordon/bin on it (zordon installs
// mise into its own bin dir, not a system location), which is why we
// inject it here rather than relying on the parent environment.
func isolatedEnv(dataDir, miseBin string) []string {
	cfgRoot := filepath.Dir(dataDir) // e.g. ~/.zordon
	env := append(zenv.Environ(),
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
// must hold the per-(tool, version) lock from Acquire — auto-install
// races otherwise.
func MiseEnv(binPath, dataDir, tool, version string, logOut io.Writer) (map[string]string, error) {
	spec := miseToolName(tool) + "@" + version
	cmd := exec.Command(binPath, "env", "--json", spec)
	cmd.Env = isolatedEnv(dataDir, binPath)
	cmd.Stderr = logOut
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mise env %s (data dir %s): %w", spec, dataDir, err)
	}
	env := map[string]string{}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("decode mise env %s: %w", spec, err)
	}
	return env, nil
}

// EnsurePackage installs a `pkg` service's native package via mise
// (`mise install <name>@<version>`). name is the mise tool ref:
// backend-qualified (e.g. "aqua:etcd-io/etcd") or a bare name that
// mise's registry resolves to a backend (redis → vfox→asdf). Idempotent
// — mise no-ops when the version is already present under dataDir.
// Download/compile progress goes to logOut (some backends build from
// source on first install). Callers hold the per-(name, version)
// Acquire lock, since the install writes under dataDir.
func EnsurePackage(binPath, dataDir, name, version string, buildEnv zenv.EnvironmentVariables, logOut io.Writer) error {
	spec := name + "@" + version
	cmd := exec.Command(binPath, "install", spec)
	// buildEnv (the pkg service's `build { env }`) overlays the install
	// environment — configure flags / build vars a source-compiled
	// backend needs, e.g. POSTGRES_EXTRA_CONFIGURE_OPTIONS=--without-icu
	// or PKG_CONFIG_PATH. Appended last so it wins over the host env.
	env := isolatedEnv(dataDir, binPath)
	for k, v := range buildEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
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
// install lands in the right interpreter's world. Idempotent in the
// sense the underlying installer decides: gem/pip/cargo no-op on a
// matching name+version that's already present.
//
// Returns the first install error or nil. Output (progress, warnings)
// goes to logOut so the user can see what's happening on first run.
func EnsureTools(binPath, dataDir, toolchain, version string, items map[string]string, logOut io.Writer) error {
	if len(items) == 0 {
		return nil
	}
	env := isolatedEnv(dataDir, binPath)
	for name, ver := range items {
		cmd, err := toolInstallCmd(binPath, toolchain, version, name, ver)
		if err != nil {
			return err
		}
		cmd.Env = env
		cmd.Stdout = logOut
		cmd.Stderr = logOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install %s tool %s@%s into %s@%s: %w", toolchain, name, ver, toolchain, version, err)
		}
	}
	return nil
}

// toolInstallCmd builds the `mise exec` line for one language-native
// tool install. The actual install verb is per-toolchain because each
// language has its own package-manager idiom.
func toolInstallCmd(binPath, toolchain, version, name, ver string) (*exec.Cmd, error) {
	spec := miseToolName(toolchain) + "@" + version
	var argv []string
	switch toolchain {
	case "ruby":
		// --no-document skips docs (faster install, fewer files).
		argv = []string{"gem", "install", name, "--version", ver, "--no-document"}
	case "python":
		argv = []string{"pip", "install", name + "==" + ver}
	case "rust":
		argv = []string{"cargo", "install", name, "--version", ver, "--locked"}
	case "go":
		// Go's "tools" aren't gems-like, but `go install pkg@ver`
		// drops a binary in GOBIN — useful for dlv, golangci-lint, etc.
		argv = []string{"go", "install", name + "@" + ver}
	case "nodejs":
		// npm's default global prefix resolves from where `node` lives,
		// which under `mise exec` is the per-version install dir
		// (installs/node/<ver>) — so installed globals stay isolated
		// per pinned node, mirroring gem/cargo's per-version tool world.
		// --no-fund/--no-audit silences npm's nag banners (the runtime
		// env's NPM_CONFIG_FUND/AUDIT in postMiseNode covers the same
		// at service-spawn time; here we still need the flag because
		// that env isn't applied at tool-install).
		argv = []string{"npm", "install", "-g", "--no-fund", "--no-audit", name + "@" + ver}
	default:
		return nil, fmt.Errorf("toolchain %q: no tool installer wired", toolchain)
	}
	full := append([]string{"exec", spec, "--"}, argv...)
	return exec.Command(binPath, full...), nil
}

// Acquire takes a process-wide exclusive lock on a lock file scoped to
// (tool, version) under dataDir. Concurrent callers in this process —
// and in any other process that mounts the same dataDir — serialize at
// this granularity, so the same Ruby's `gem install bundler` can't run
// twice in parallel and corrupt the tarball-extract.
//
// The returned release closes the lock file (which drops the flock
// implicitly). Callers wrap EnsureTools and MiseEnv together inside one
// Acquire so the same locked critical section covers both installing
// tools and reading env (which itself may auto-install the version).
func Acquire(dataDir, tool, version string) (release func(), err error) {
	return zfs.AcquireLock(filepath.Join(dataDir, "locks"), sanitizeForPath(tool)+"-"+version)
}

// sanitizeForPath makes a mise tool ref safe as a single filesystem path
// component: a backend-qualified pkg ref like "aqua:etcd-io/etcd" carries
// ':' and '/' that would otherwise nest into a non-existent directory or
// break a lock-file path. Language refs (go/rust/...) are unchanged.
func sanitizeForPath(s string) string {
	return strings.NewReplacer("/", "-", ":", "-").Replace(s)
}
