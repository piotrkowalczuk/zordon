// Package zordontest is the conformance test harness: a black-box
// driver that creates a real project on disk, runs the zordon binary
// against it, and lets the test assert on observable side-effects
// (service HTTP responses, files, exit codes, registry contents).
//
// The harness is deliberately language-agnostic — it neither knows
// nor cares whether a service is go / ruby / rust / anything else.
// The TEST writes the Alphasfile; the harness just drives zordon
// and reports back.
//
// Design constraints encoded here:
//
//   - Real filesystem (t.TempDir, no in-memory mocks).
//   - Outside zordon's own repo (everything lives in tmpdir).
//   - No t.Parallel — resources are too constrained.
//   - No skip-on-missing — first invocation bootstraps everything;
//     subsequent runs amortize via the shared cache (see #55).
//   - Cleanup never uses pkill — it goes through `zordon stop`
//     plus the registry-based safety net (see #57). Tests that
//     deliberately kill processes are their own scenarios.
package zordontest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Project is one temp-dir-rooted project under harness control.
// Lifetime is bounded by t.Cleanup — when the test ends, the harness
// stops alpha gracefully and removes the temp tree.
type Project struct {
	t       *testing.T
	root    string // absolute path to the project tmpdir
	home    string // ZORDON_HOME for this project (shared cache by default)
	binZ    string // path to the `zordon` binary
	testLog string // ZORDON_TEST_LOG — where test:: HCL fns append observations
	cfg     config // resolved options
}

// NewProject creates a fresh project tmpdir and registers cleanup.
// Returns a *Project ready for CopyTree / WriteFile / Zordon calls.
//
// Cleanup order on test end:
//
//  1. Attempt `zordon stop` with a short timeout (graceful path,
//     alpha reaps its own children via killGroup).
//  2. Remove the project tmpdir.
//
// Step 1 may fail (alpha already dead, socket gone, etc.) — that's
// fine, the registry-based reaper inside the next alpha startup is
// the ultimate safety net. We do NOT pkill here.
func NewProject(t *testing.T, opts ...Option) *Project {
	t.Helper()
	cfg := resolveConfig(opts)

	root := cfg.root
	if root == "" {
		root = t.TempDir()
	}
	binZ := resolveZordonBinary(t)
	home := resolveHome(t, cfg)

	p := &Project{
		t:       t,
		root:    root,
		home:    home,
		binZ:    binZ,
		testLog: filepath.Join(root, "_test.log"),
		cfg:     cfg,
	}
	t.Cleanup(p.cleanup)
	return p
}

// TestLog returns the lines appended by `test::log(tag)` HCL calls
// during this project's zordon invocations, in append order. Empty
// slice if no test:: function ever ran (or if the test never used
// the test:: namespace).
//
// Use this to assert chain ordering — e.g., write provisions with
// `cmd = test::log("p1")` / `cmd = test::log("p2")` chained via
// `after`, then check that lines == ["p1", "p2"].
func (p *Project) TestLog() []string {
	p.t.Helper()
	data, err := os.ReadFile(p.testLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		p.t.Fatalf("read test log %s: %v", p.testLog, err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// Dir returns the project root (where Alphasfile lives by default).
// All relative paths passed to CopyTree / WriteFile / MkdirAll are
// resolved against this.
func (p *Project) Dir() string { return p.root }

// Home returns the ZORDON_HOME the project's zordon invocations see.
// Useful for asserting on registry.json, toolchain cache contents,
// etc.
func (p *Project) Home() string { return p.home }

// AlphaLogPath returns the canonical alpha log location for this
// project. Tests that invoke `zordon start` should pass it through:
//
//	p.Zordon("start", "--alpha-log", p.AlphaLogPath()).Run(t)
//
// so the log lands somewhere AlphaLog() can read back. We don't
// inject it automatically — `Zordon(...)` is a generic CLI runner
// and shouldn't second-guess the user's argv.
func (p *Project) AlphaLogPath() string {
	return filepath.Join(p.root, "alpha.log")
}

// Get evaluates a zordon-resolved expression (dotted path or Go
// template) by shelling out to `zordon get <expr>` and returns the
// trimmed stdout. Fails the test on non-zero exit — the typical
// use is to resolve a vars-derived value (port, derived URL, etc.)
// for an HTTP assertion, and a missing expression is a real bug.
func (p *Project) Get(expr string) string {
	p.t.Helper()
	res := p.Zordon("get", expr).Run(p.t)
	if res.ExitCode != 0 {
		p.t.Fatalf("zordon get %q: exit %d\nstdout: %s\nstderr: %s",
			expr, res.ExitCode, res.Stdout, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// CopyTree recursively copies a source directory into the project,
// preserving file modes. srcRel is relative to the zordon repo root
// (the location of this test binary's package), so tests can reference
// fixtures like "golden/go/echo". dstRel is relative to the project
// root.
//
// Symlinks are followed; their target content is copied. .git
// directories ARE copied (some tests need them); use WriteFile to
// override or remove afterwards.
func (p *Project) CopyTree(srcRel, dstRel string) *Project {
	p.t.Helper()
	src := resolveFixturePath(p.t, srcRel)
	dst := filepath.Join(p.root, dstRel)
	if err := copyTree(src, dst); err != nil {
		p.t.Fatalf("CopyTree(%q → %q): %v", srcRel, dstRel, err)
	}
	return p
}

// WriteFile creates relPath (with intermediate dirs) and writes
// content. Overwrites any existing file. Returns Project so calls
// can chain.
func (p *Project) WriteFile(relPath, content string) *Project {
	p.t.Helper()
	// WithToolchain: prepend the flag-pinned Go toolchain block so
	// fixtures stay plain HCL. HCL block order is irrelevant, so a
	// straight prefix is safe.
	if p.cfg.injectGoToolchain && filepath.Base(relPath) == "Alphasfile" {
		content = goToolchainHCL() + content
	}
	full := filepath.Join(p.root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatalf("WriteFile(%q): mkdir parent: %v", relPath, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		p.t.Fatalf("WriteFile(%q): %v", relPath, err)
	}
	return p
}

// MkdirAll creates relPath (and intermediate dirs) under the project
// root. No-op if it already exists.
func (p *Project) MkdirAll(relPath string) *Project {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Join(p.root, relPath), 0o755); err != nil {
		p.t.Fatalf("MkdirAll(%q): %v", relPath, err)
	}
	return p
}

// cleanup is wired via t.Cleanup. Three phases:
//
//  1. zordon stop — best-effort graceful path; alpha killGroup's
//     children, registry entries get removed by the supervisor's
//     defer.
//  2. AssertNoLeftovers — verifies nothing from this run is still
//     registered or holding a socket. On a hit it ALSO reaps so
//     the next test starts clean (better than letting state cascade).
//     WithExpectedLeftovers swaps this for a silent Nuke(): the test
//     provably precludes cleanup, so a leftover is expected, not a bug.
//  3. For WithExistingRoot only: remove `<root>/.zordon/` — the
//     per-run state alpha materialized inside the example tree.
//     tmpdir-rooted projects get this for free via t.TempDir.
func (p *Project) cleanup() {
	_ = p.zordonStopBestEffort()
	if p.cfg.expectLeftovers {
		p.Nuke()
	} else {
		p.AssertNoLeftovers(p.t)
	}
	if p.cfg.root != "" {
		// Only when the user opted into WithExistingRoot — otherwise
		// the root IS t.TempDir() and Go cleans it itself.
		_ = os.RemoveAll(filepath.Join(p.root, ".zordon"))
	}
}

func (p *Project) zordonStopBestEffort() error {
	cmd := exec.Command(p.binZ, "stop")
	cmd.Dir = p.root
	cmd.Env = p.env()
	// stop is fast (the slow part is service teardown, which we don't
	// need to wait for during cleanup — the registry will reap on
	// next start). Bound it to a few seconds and swallow output.
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// env produces the env slice every zordon invocation from this project
// runs with: filtered host env plus ZORDON_HOME + TMPDIR.
//
// We keep PATH from the host so the binary can find git, mise, etc.
// — production zordon strips most of os.Environ() via Alphasfile.sysenv
// at SERVICE spawn time, not at zordon-itself spawn time. The harness
// drives the zordon CLI, not a service.
func (p *Project) env() []string {
	out := append([]string{}, os.Environ()...)
	out = setEnv(out, "ZORDON_HOME", p.home)
	// Always-on test harness markers: every zordon CLI call this
	// project makes runs with the test:: namespace unlocked and
	// pointed at this project's log file. Spawned services don't
	// inherit these unless the Alphasfile whitelists them in sysenv;
	// test::log/test::fail bake the log path in at HCL eval time
	// so they keep working anyway.
	out = setEnv(out, "ZORDON_TEST_HARNESS", "1")
	out = setEnv(out, "ZORDON_TEST_LOG", p.testLog)
	return out
}

// resolveZordonBinary locates the `zordon` executable for the harness
// to invoke. Production tests want a real built binary; we don't try
// to build it from source here (callers bootstrap before running the
// suite — see Slice 4 for the shared-cache builder).
//
// Resolution: $ZORDON_BIN env (absolute path) > `zordon` on $PATH.
// Fails the test if neither resolves — there's no graceful fallback
// since the harness's whole job is to drive that binary.
func resolveZordonBinary(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("ZORDON_BIN"); v != "" {
		return v
	}
	bin, err := exec.LookPath("zordon")
	if err != nil {
		t.Fatalf("zordon binary not found: set $ZORDON_BIN or `go install ./cmd/zordon` first")
	}
	return bin
}

// DefaultHome is the shared ZORDON_HOME path the harness gives every
// project unless WithIsolatedHome opts out. Lives at <repo>/.zordon/
// — the canonical zordon-state location — so:
//
//   - mise + toolchain installs amortize across the suite (first test
//     pays the bootstrap cost, the rest reuse). Production paths are
//     already idempotent (EnsureMise no-ops on existing binary) and
//     serialized (tools.Acquire flocks per-(lang, version)), so the
//     shared dir is safe under cross-package concurrent writes.
//   - `.zordon/` matches the convention every other zordon dir uses
//     (~/.zordon, <project>/.zordon/worktrees). A new contributor
//     recognizes it instantly.
//   - The existing `.gitignore` pattern covers it without a new line.
//   - Walk-up bound (filepath.Dir(<repo>/.zordon) = <repo>) is the
//     repo root, so federation discovery and toolchain walk-up never
//     escape into the developer's real ~/.zordon.
//
// DefaultHome does NOT create the directory — call it from a place
// that ensures the path exists (NewProject does this via resolveHome).
// Exposed so tests asserting on "the shared home" don't have to
// re-derive the path.
func DefaultHome(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), ".zordon")
}

// resolveHome picks ZORDON_HOME for the project: the cfg override
// when set (WithIsolatedHome), otherwise DefaultHome with the
// directory eagerly created.
//
// Tests that need a guaranteed-empty home (first-install scenarios,
// registry-isolation checks) opt out via WithIsolatedHome.
func resolveHome(t *testing.T, cfg config) string {
	t.Helper()
	if cfg.home != "" {
		return cfg.home
	}
	home := DefaultHome(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create shared zordon home: %v", err)
	}
	return home
}

// setEnv replaces or appends KEY=value in a flat env slice.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
