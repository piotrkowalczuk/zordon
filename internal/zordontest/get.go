package zordontest

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// FreePort asks the kernel for an unused TCP port and returns it. Use it
// when the test must KNOW the port up front and can't read it back from a
// running alpha — e.g. lifecycle/orphan tests that probe `portServing`
// while alpha is being killed. There's a tiny window between this call and
// the service binding, but the kernel hands out a currently-free port, so
// it can't collide with a peer the way a shared hardcoded literal does.
func FreePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Value is a resolved `zordon get` result with typed accessors, so a
// test reads a port as an int (or a path as a string) without poking at
// strconv and error tuples at the call site.
type Value struct {
	t    testing.TB
	expr string
	raw  string
}

// String is the resolved value, trimmed.
func (v Value) String() string { return v.raw }

// Int parses the resolved value as an int (a port, a count). Fatal if it
// isn't one — a non-numeric value where the test expected a number is a
// real bug, not something to paper over.
func (v Value) Int() int {
	v.t.Helper()
	n, err := strconv.Atoi(v.raw)
	if err != nil {
		v.t.Fatalf("zordon get %q = %q, want an int: %v", v.expr, v.raw, err)
	}
	return n
}

// Get resolves expr against the running chain (or a static eval if no
// alpha is up) via `zordon get` and returns a typed Value. Fatal if the
// expression doesn't resolve — a missing path is a real bug. Pass
// testing.TB explicitly so the helper works from a subtest's t.
//
// Because get reads the RUNNING alpha's resolved state, a port chosen by
// net::pickport() comes back as the port the service actually bound —
// which is how a test learns a dynamic port instead of hardcoding one.
func (p *Project) Get(t testing.TB, expr string) Value {
	t.Helper()
	return p.get(t, "", expr)
}

// GetIn is Get run from relDir (relative to the project root) — use it to
// resolve a service in a worktree subtree, whose alpha lives under that
// worktree's state dir and isn't visible from the project root.
func (p *Project) GetIn(t testing.TB, relDir, expr string) Value {
	t.Helper()
	return p.get(t, relDir, expr)
}

func (p *Project) get(t testing.TB, relDir, expr string) Value {
	t.Helper()
	cmd := p.Zordon("get", expr)
	if relDir != "" {
		cmd = cmd.WithDir(relDir)
	}
	res := cmd.Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon get %q: exit %d\nstdout: %s\nstderr: %s",
			expr, res.ExitCode, res.Stdout, res.Stderr)
	}
	return Value{t: t, expr: expr, raw: strings.TrimSpace(res.Stdout)}
}
