// Package ztest holds test-only policy shared by every package's tests.
package ztest

import (
	"fmt"
	"os"
	"testing"
)

// NoSkipEnv turns every Skip into a failure. CI sets it: a skipped test
// proves nothing, and `go test` without -v does not print skips at all, so
// a missing tool would otherwise hide a whole suite behind a green run.
const NoSkipEnv = "ZORDON_NO_SKIP"

// Skip skips the test locally and fails it when NoSkipEnv is "1". It is the
// only sanctioned way to skip: `make lint` rejects a direct t.Skip*.
func Skip(t testing.TB, format string, args ...any) {
	t.Helper()
	reason := fmt.Sprintf(format, args...)
	if Strict() {
		t.Fatalf("skip forbidden (%s=1): %s", NoSkipEnv, reason)
	}
	t.Skip(reason)
}

// Strict reports whether skips are forbidden in this run.
func Strict() bool {
	// os, not zenv: zenv imports zfs, and zfs's own tests use this package.
	return os.Getenv(NoSkipEnv) == "1"
}
