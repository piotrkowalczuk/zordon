// Package ztest checks that the host can run a test at all. A missing
// prerequisite fails the test with what is missing and why; tests never
// skip.
package ztest

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Need is one thing a test requires from the host.
type Need struct {
	name  string
	why   string
	ready func() bool
}

// Git is required by every test AssertSystem guards: each source and each
// toolchain goes through git, so AssertSystem always checks it.
var Git = Need{
	name:  "git",
	why:   "every git/src source and every toolchain is materialized through git; install git",
	ready: onPath("git"),
}

// Python3 is required by tests that spawn a helper process with it.
var Python3 = Need{
	name:  "python3",
	why:   "the test spawns a helper process with python3; install python3",
	ready: onPath("python3"),
}

// Toolchain is required by every test that brings up a pinned toolchain.
// alpha materializes toolchains through mise and bootstraps mise itself
// from <zordonHome>/bin/mise, falling back to `cargo install mise`.
func Toolchain(zordonHome string) Need {
	placed := filepath.Join(zordonHome, "bin", "mise")
	return Need{
		name: "mise",
		why:  fmt.Sprintf("toolchains install through mise, which must be pre-placed at %s or buildable with cargo; place mise there (CI does) or install cargo", placed),
		ready: func() bool {
			if st, err := zfs.Stat(placed); err == nil && !st.IsDir() {
				return true
			}
			return onPath("cargo")()
		},
	}
}

// AssertSystem fails the test unless the host provides Git and every need
// given, listing all that are missing at once.
func AssertSystem(t testing.TB, needs ...Need) {
	t.Helper()
	var missing []string
	for _, n := range append([]Need{Git}, needs...) {
		if !n.ready() {
			missing = append(missing, fmt.Sprintf("  - %s: %s", n.name, n.why))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("system not ready for this test:\n%s", strings.Join(missing, "\n"))
	}
}

func onPath(bin string) func() bool {
	return func() bool {
		_, err := exec.LookPath(bin)
		return err == nil
	}
}
