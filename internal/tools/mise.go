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
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		return "", fmt.Errorf("zordon needs mise to materialize declared toolchains; "+
			"install cargo so zordon can `cargo install` it to %s, "+
			"or pre-place a mise binary there yourself", bin)
	}
	if err := os.MkdirAll(filepath.Join(zordonHome, "bin"), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s/bin: %w", zordonHome, err)
	}
	// Serialize cargo install so two alphas starting at the same time
	// don't try to write the same binary. Re-check existence under the
	// lock — the other waiter just did it for us.
	release, err := lockFile(filepath.Join(zordonHome, "locks", "mise.lock"))
	if err != nil {
		return "", err
	}
	defer release()
	if _, err := os.Stat(bin); err == nil {
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

// lockFile is the shared LOCK_EX primitive used by EnsureMise and
// Acquire. The lock is held for the lifetime of the returned closure;
// callers `defer release()`.
func lockFile(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
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
	bound := filepath.Dir(defaultDataDir)
	dir := from
	for {
		candidate := filepath.Join(dir, ".zordon", "toolchain")
		if _, err := os.Stat(filepath.Join(candidate, "installs", tool, version)); err == nil {
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
// owned location regardless of user's home mise installation.
func isolatedEnv(dataDir string) []string {
	cfgRoot := filepath.Dir(dataDir) // e.g. ~/.zordon
	return append(os.Environ(),
		"MISE_DATA_DIR="+dataDir,
		"MISE_CONFIG_DIR="+filepath.Join(cfgRoot, "mise-config"),
		"MISE_STATE_DIR="+filepath.Join(cfgRoot, "mise-state"),
		"MISE_CACHE_DIR="+filepath.Join(cfgRoot, "mise-cache"),
	)
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
	spec := tool + "@" + version
	cmd := exec.Command(binPath, "env", "--json", spec)
	cmd.Env = isolatedEnv(dataDir)
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
	for name, ver := range items {
		cmd, err := toolInstallCmd(binPath, toolchain, version, name, ver)
		if err != nil {
			return err
		}
		cmd.Env = isolatedEnv(dataDir)
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
	spec := toolchain + "@" + version
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
	return lockFile(filepath.Join(dataDir, "locks", tool+"-"+version+".lock"))
}
