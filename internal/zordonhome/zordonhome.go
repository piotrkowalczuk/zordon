// Package zordonhome is the single source of truth for "where does
// zordon keep its host-wide state" — the directory that holds the
// shared mise install, the toolchain cache, the port registry, and
// the bare-clone mirrors for git-pinned services.
//
// Resolution order:
//
//  1. Explicit override (caller passes a flag-derived value).
//  2. $ZORDON_HOME env var.
//  3. <UserHomeDir>/.zordon as the production default.
//
// Every binary and library that needs this path MUST go through
// Resolve so the test harness can redirect it with a single env var
// (or override) and every layer below converges on the same dir.
package zordonhome

import (
	"os"
	"path/filepath"
)

// Resolve picks the zordon home dir, preferring the explicit override
// when non-empty, then $ZORDON_HOME, then <UserHomeDir>/.zordon.
// Returns "" only when UserHomeDir itself fails AND no override /
// env var is set — callers degrade gracefully (no registry, no
// shared cache) rather than panicking.
func Resolve(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("ZORDON_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".zordon")
}
