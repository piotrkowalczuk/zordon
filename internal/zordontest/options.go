package zordontest

import "github.com/piotrkowalczuk/zordon/internal/alphasfile"

// Option configures a Project at construction time. Functional-options
// pattern keeps NewProject's signature stable as we add knobs (agent
// mode, isolated cache, federation depth, etc.) without breaking
// existing tests.
type Option func(*config)

// config is the resolved option bag. Internal; tests don't touch
// this directly.
type config struct {
	// home overrides ZORDON_HOME for this project. When empty, the
	// harness uses the shared cache location (Slice 4).
	home string
	// root overrides the project root. When empty, NewProject creates
	// a fresh t.TempDir(). Use WithExistingRoot to point at an
	// already-laid-out tree (e.g. an in-repo example/) — the harness
	// still cleans up `.zordon/` state subdirs at end of test.
	root string
	// expectLeftovers opts the project out of the strict
	// AssertNoLeftovers cleanup check. See WithExpectedLeftovers.
	expectLeftovers bool
	// injectGoToolchain prepends a Go toolchain block (version from
	// the -test.toolchain.go.version flag) to any Alphasfile this
	// project writes. goToolchain carries the block's inner contents
	// (tools/env); its Version is ignored — the flag always wins.
	// See WithToolchain.
	injectGoToolchain bool
	goToolchain       alphasfile.ToolchainConfig
}

// WithIsolatedHome forces the project to use the given path as its
// ZORDON_HOME instead of the shared cache. Use for tests that
// verify first-run install behavior, registry isolation across
// projects, or anything else that needs a guaranteed-empty home.
//
// Trade-off: tests opting in pay the full bootstrap cost (mise
// install + toolchain downloads) on every run. Use sparingly.
func WithIsolatedHome(path string) Option {
	return func(c *config) { c.home = path }
}

// WithExistingRoot points the project at an existing directory
// (typically an in-repo `examples/<name>/` tree) instead of a
// fresh t.TempDir(). Useful when the example's Alphasfile uses
// relative paths that are meaningful in-place (e.g. src = "../..").
//
// The harness still cleans up `<root>/.zordon/` (per-project state
// alpha writes during a run) at test end, and AssertNoLeftovers
// still verifies nothing from this run survived. The user-authored
// Alphasfile and source files are untouched.
func WithExistingRoot(path string) Option {
	return func(c *config) { c.root = path }
}

// WithExpectedLeftovers declares that this test, by its very nature,
// leaves registry/state behind that no graceful path will clean — so
// the harness should Nuke() the project's state at cleanup instead of
// failing AssertNoLeftovers.
//
// This is NOT a way to silence a flaky leftover. Use it only when the
// scenario provably precludes cleanup, e.g. the test SIGKILLs the
// supervisor (alpha) so its registry-deregister code never runs; in
// production that row is reaped by the next `zordon start`, which a
// single-shot test doesn't perform. Every other test stays strict.
func WithExpectedLeftovers() Option {
	return func(c *config) { c.expectLeftovers = true }
}

// WithToolchain opts the project into Go toolchain injection, BUT the
// -test.toolchain.go.version flag (or $ZORDON_TEST_GO_VERSION) is the
// actual switch: with neither set, nothing is injected and fixtures
// run against the ambient Go; with it set, a minimal
// `toolchain { go { version } }` is prepended to every Alphasfile the
// project writes. The version is ALWAYS the flag's — the optional
// alphasfile.ToolchainConfig only sets what's INSIDE the block
// (Tools/Env); its Version field is ignored.
//
// The block is serialized from a typed value via gohcl and
// deserialized by zordon's own decoder — a real round-trip, not a
// hand-built string. Tests that must never get a toolchain (e.g. a
// forced build-failure that shouldn't wait on mise) simply omit this
// option entirely.
func WithToolchain(tc ...alphasfile.ToolchainConfig) Option {
	return func(c *config) {
		c.injectGoToolchain = true
		if len(tc) > 0 {
			c.goToolchain = tc[0]
		}
	}
}

// resolveConfig folds the option chain into a config. Each option
// runs in declaration order so later options can override earlier
// ones — same semantics as fx, ff, etc.
func resolveConfig(opts []Option) config {
	var c config
	for _, opt := range opts {
		opt(&c)
	}
	return c
}
