package zordontest

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
