package tools

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// EnsureNodeCorepack installs a Corepack that supports the pinned Node into
// a per-(node, corepack) dir under dataDir and runs `corepack enable` so
// pnpm/yarn shims are available to services. Without a refresh, the
// Corepack bundled inside older Node distributions ships a stale signing-key
// set and `pnpm install` fails with `Cannot find matching keyid` against
// current pnpm releases.
//
// The refreshed Corepack is the newest series whose `engines.node` accepts
// the actual Node version (see CorepackSpecFor), and env gets
// COREPACK_DEFAULT_TO_LATEST=0: a project that pins no packageManager then
// runs the package manager version that Corepack release ships as its
// default, which targets the same Node range, instead of whatever is latest
// on the registry (pnpm 11+ needs Node >= 22.13). COREPACK_HOME is private
// to the (node, corepack) pair: the shared default home keeps a
// lastKnownGood.json that outranks the bundled default, so a pnpm recorded
// there by a newer Node line would leak into older ones.
//
// Idempotent: if the shims and refreshed corepack already exist (e.g. a
// previous alpha run materialized this toolchain), we skip the install +
// enable. The caller must hold the per-(tool, version) lock from Acquire to
// guard concurrent first-runs.
//
// On success, mutates env: prepends the shim dir AND the refreshed-corepack
// bin dir to PATH so service spawns find the zordon-managed shims ahead of
// node's own bundled corepack. The user's host PATH stays untouched.
func EnsureNodeCorepack(binPath, dataDir, version string, env map[string]string, logOut io.Writer) error {
	spec := "node@" + version
	nodeVersion, err := nodeRuntimeVersion(binPath, dataDir, spec)
	if err != nil {
		return err
	}
	corepack, ok := CorepackSpecFor(nodeVersion)
	if !ok {
		return fmt.Errorf("node %s: no Corepack release supports it; pin Node 18.17.1 or newer", nodeVersion)
	}

	refreshRoot := filepath.Join(dataDir, "node-corepack", version, strings.ReplaceAll(corepack, "@", "-"))
	shimDir := filepath.Join(refreshRoot, "shims")
	corepackBin := filepath.Join(refreshRoot, "bin", "corepack")

	env["COREPACK_DEFAULT_TO_LATEST"] = "0"
	env["COREPACK_HOME"] = filepath.Join(refreshRoot, "home")

	pnpmShim := filepath.Join(shimDir, "pnpm")
	if zfs.Exists(corepackBin) && zfs.Exists(pnpmShim) {
		injectNodeCorepackPATH(env, shimDir, filepath.Dir(corepackBin))
		return nil
	}

	if err := zfs.EnsureDir(refreshRoot); err != nil {
		return err
	}
	if err := zfs.EnsureDir(shimDir); err != nil {
		return err
	}

	// Refreshed corepack lands in refreshRoot/{bin,lib} (npm's --prefix
	// layout). isolatedEnv puts the mise binary on PATH so the
	// mise-installed node's npm wrapper, which calls `mise reshim`
	// post-install, can find mise itself.
	install := miseCommand(binPath, dataDir, "exec", spec, "--",
		"npm", "install", "-g",
		"--prefix", refreshRoot,
		"--no-fund", "--no-audit",
		"--engine-strict",
		corepack)
	install.Stdout = logOut
	install.Stderr = logOut
	if err := install.Run(); err != nil {
		return fmt.Errorf("install %s into %s: %w", corepack, refreshRoot, err)
	}

	// `corepack enable` writes pnpm/pnpx/yarn/yarnpkg shims to shimDir.
	// Run via mise exec so the refreshed corepack finds node on PATH.
	enable := miseCommand(binPath, dataDir, "exec", spec, "--",
		corepackBin, "enable",
		"--install-directory", shimDir)
	enable.Stdout = logOut
	enable.Stderr = logOut
	if err := enable.Run(); err != nil {
		return fmt.Errorf("corepack enable into %s: %w", shimDir, err)
	}

	injectNodeCorepackPATH(env, shimDir, filepath.Dir(corepackBin))
	return nil
}

// CorepackSpecFor returns the npm spec of the newest Corepack series whose
// engines.node accepts nodeVersion ("22.11.0", with or without a leading
// "v"), and false when no series does. The table mirrors the engines field
// published on the registry; a series is listed once its range changes.
func CorepackSpecFor(nodeVersion string) (string, bool) {
	v, ok := parseNodeVersion(nodeVersion)
	if !ok {
		return "", false
	}
	for _, s := range corepackSeries {
		if s.supports(v) {
			return s.spec, true
		}
	}
	return "", false
}

type nodeVersion [3]int

type corepackRelease struct {
	spec string
	// supports mirrors the release's engines.node range.
	supports func(nodeVersion) bool
}

// corepackSeries is newest first. Engines as published:
//
//	0.35–0.36  ^22.22.2 || ^24.15.0 || >=26.0.0
//	0.34       ^20.10.0 || ^22.11.0 || >=24.0.0
//	0.31–0.33  ^18.17.1 || ^20.10.0 || >=22.11.0
var corepackSeries = []corepackRelease{
	{spec: "corepack@~0.36.0", supports: func(v nodeVersion) bool {
		return caret(v, 22, 22, 2) || caret(v, 24, 15, 0) || v.atLeast(26, 0, 0)
	}},
	{spec: "corepack@~0.34.0", supports: func(v nodeVersion) bool {
		return caret(v, 20, 10, 0) || caret(v, 22, 11, 0) || v.atLeast(24, 0, 0)
	}},
	{spec: "corepack@~0.33.0", supports: func(v nodeVersion) bool {
		return caret(v, 18, 17, 1) || caret(v, 20, 10, 0) || v.atLeast(22, 11, 0)
	}},
}

func (v nodeVersion) atLeast(major, minor, patch int) bool {
	o := nodeVersion{major, minor, patch}
	for i := range v {
		if v[i] != o[i] {
			return v[i] > o[i]
		}
	}
	return true
}

// caret is npm's ^M.m.p for M > 0: same major, at least M.m.p.
func caret(v nodeVersion, major, minor, patch int) bool {
	return v[0] == major && v.atLeast(major, minor, patch)
}

func parseNodeVersion(s string) (nodeVersion, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return nodeVersion{}, false
	}
	var v nodeVersion
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nodeVersion{}, false
		}
		v[i] = n
	}
	return v, true
}

// nodeRuntimeVersion asks the mise-installed node for its exact version: a
// pin may be fuzzy ("22"), and Corepack's support is decided by patch level.
func nodeRuntimeVersion(binPath, dataDir, spec string) (string, error) {
	out, err := miseCommand(binPath, dataDir, "exec", spec, "--", "node", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s: node --version: %w", spec, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// injectNodeCorepackPATH prepends the shim dir and refreshed-corepack
// bin dir to env["PATH"] so services see the zordon-managed shims and
// corepack ahead of whatever node ships bundled.
func injectNodeCorepackPATH(env map[string]string, shimDir, corepackBinDir string) {
	prefix := shimDir + string(zfs.PathListSeparator) + corepackBinDir
	if cur := env["PATH"]; cur != "" {
		env["PATH"] = prefix + string(zfs.PathListSeparator) + cur
	} else {
		env["PATH"] = prefix
	}
}
