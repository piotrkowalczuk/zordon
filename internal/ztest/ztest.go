// Package ztest checks that the host can run zordon's tests at all. A
// missing prerequisite fails the test with what is missing and why; tests
// never skip.
package ztest

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Need is one extra thing a test requires from the host, on top of the
// system every test gets checked for.
type Need struct {
	name  string
	why   string
	ready func() bool
}

// Python3 is required by tests that spawn a helper process with it.
var Python3 = Need{
	name:  "python3",
	why:   "the test spawns a helper process with python3; install python3",
	ready: onPath("python3"),
}

// AssertSystem fails the test unless the host has what zordon itself stands
// on, git and mise, plus every extra need given. Every missing piece is
// listed at once with its reason.
func AssertSystem(t testing.TB, needs ...Need) {
	t.Helper()
	all := append([]Need{gitNeed, miseNeed(Home(t))}, needs...)
	var missing []string
	for _, n := range all {
		if !n.ready() {
			missing = append(missing, fmt.Sprintf("  - %s: %s", n.name, n.why))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("system not ready for zordon's tests:\n%s", strings.Join(missing, "\n"))
	}
}

// Home is the ZORDON_HOME tests share: <repo>/.zordon. CI pre-places mise
// there, and every toolchain install caches under it.
func Home(t testing.TB) string {
	t.Helper()
	return filepath.Join(repoRoot(t), ".zordon")
}

var gitNeed = Need{
	name:  "git",
	why:   "every git/src source and every toolchain is materialized through git; install git",
	ready: onPath("git"),
}

// miseNeed is mise, the base every toolchain and package installs through.
// zordon runs <home>/bin/mise and builds it with `cargo install mise` when
// it is missing, so either makes the host ready.
func miseNeed(home string) Need {
	placed := filepath.Join(home, "bin", "mise")
	return Need{
		name: "mise",
		why:  fmt.Sprintf("zordon installs every toolchain and package through mise; place it at %s (CI does) or install cargo so zordon can build it", placed),
		ready: func() bool {
			if st, err := zfs.Stat(placed); err == nil && !st.IsDir() {
				return true
			}
			return onPath("cargo")()
		},
	}
}

func onPath(bin string) func() bool {
	return func() bool {
		_, err := exec.LookPath(bin)
		return err == nil
	}
}

var (
	rootOnce sync.Once
	rootDir  string
)

// repoRoot walks up from this source file to the directory holding go.mod.
func repoRoot(t testing.TB) string {
	t.Helper()
	rootOnce.Do(func() {
		_, here, _, ok := runtime.Caller(0)
		if !ok {
			return
		}
		for dir := filepath.Dir(here); ; dir = filepath.Dir(dir) {
			if zfs.Exists(filepath.Join(dir, "go.mod")) {
				rootDir = dir
				return
			}
			if filepath.Dir(dir) == dir {
				return
			}
		}
	})
	if rootDir == "" {
		t.Fatal("ztest: no go.mod above the ztest package")
	}
	return rootDir
}
