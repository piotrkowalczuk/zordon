package tools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureNodeCorepack installs a fresh Corepack into a per-(node-version)
// dir under dataDir and runs `corepack enable` so pnpm/yarn shims are
// available to services. Without this refresh, the Corepack bundled
// inside older Node distributions ships a stale signing-key set and
// `pnpm install` fails with `Cannot find matching keyid` against
// current pnpm releases — see internal/tools/corepack_test.go for the
// empirical matrix.
//
// Idempotent: if the shims and refreshed corepack already exist (e.g.
// a previous alpha run materialized this toolchain), we skip the
// install + enable. The caller must hold the per-(tool, version) lock
// from Acquire to guard concurrent first-runs.
//
// On success, mutates env: prepends the shim dir AND the refreshed-
// corepack bin dir to PATH so service spawns find the zordon-managed
// shims ahead of node's own bundled corepack. The user's host PATH
// stays untouched.
func EnsureNodeCorepack(binPath, dataDir, version string, env map[string]string, logOut io.Writer) error {
	refreshRoot := filepath.Join(dataDir, "node-corepack", version)
	shimDir := filepath.Join(refreshRoot, "shims")
	corepackBin := filepath.Join(refreshRoot, "bin", "corepack")

	pnpmShim := filepath.Join(shimDir, "pnpm")
	if _, err := os.Stat(corepackBin); err == nil {
		if _, err := os.Stat(pnpmShim); err == nil {
			injectNodeCorepackPATH(env, shimDir, filepath.Dir(corepackBin))
			return nil
		}
	}

	if err := os.MkdirAll(refreshRoot, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", refreshRoot, err)
	}
	if err := os.MkdirAll(shimDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", shimDir, err)
	}

	spec := "node@" + version

	// Refreshed corepack lands in refreshRoot/{bin,lib} (npm's --prefix
	// layout). isolatedEnv puts the mise binary on PATH so the
	// mise-installed node's npm wrapper, which calls `mise reshim`
	// post-install, can find mise itself.
	install := exec.Command(binPath, "exec", spec, "--",
		"npm", "install", "-g",
		"--prefix", refreshRoot,
		"--no-fund", "--no-audit",
		"corepack@latest")
	install.Env = isolatedEnv(dataDir, binPath)
	install.Stdout = logOut
	install.Stderr = logOut
	if err := install.Run(); err != nil {
		return fmt.Errorf("install corepack@latest into %s: %w", refreshRoot, err)
	}

	// `corepack enable` writes pnpm/pnpx/yarn/yarnpkg shims to shimDir.
	// Run via mise exec so the refreshed corepack finds node on PATH.
	enable := exec.Command(binPath, "exec", spec, "--",
		corepackBin, "enable",
		"--install-directory", shimDir)
	enable.Env = isolatedEnv(dataDir, binPath)
	enable.Stdout = logOut
	enable.Stderr = logOut
	if err := enable.Run(); err != nil {
		return fmt.Errorf("corepack enable into %s: %w", shimDir, err)
	}

	injectNodeCorepackPATH(env, shimDir, filepath.Dir(corepackBin))
	return nil
}

// injectNodeCorepackPATH prepends the shim dir and refreshed-corepack
// bin dir to env["PATH"] so services see the zordon-managed shims and
// corepack ahead of whatever node ships bundled.
func injectNodeCorepackPATH(env map[string]string, shimDir, corepackBinDir string) {
	prefix := shimDir + string(os.PathListSeparator) + corepackBinDir
	if cur := env["PATH"]; cur != "" {
		env["PATH"] = prefix + string(os.PathListSeparator) + cur
	} else {
		env["PATH"] = prefix
	}
}
