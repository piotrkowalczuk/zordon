package zordontest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/registry"
)

// AssertNoLeftovers fails the test if anything from this project's
// run is still alive after a graceful stop. The harness runs this
// automatically at cleanup (so a misbehaving test trips the alarm
// without needing an explicit call), but it's also exposed publicly
// so a test can wedge it mid-flow ("at this point no provisions
// should have spawned anything yet").
//
// What counts as a leftover:
//
//  1. Registry entries whose FsHash matches this project — those
//     are services that registered themselves at spawn and were
//     never deregistered, meaning either alpha died unexpectedly
//     or graceful stop didn't reap a child.
//  2. The per-project state dir (<root>/.zordon/) still containing
//     a socket or live worktree — secondary signal; should be
//     covered by (1) but useful for non-registry-using services.
//
// On a hit, the harness ALSO best-effort reaps via registry so the
// next test isn't polluted by this run's mess.
func (p *Project) AssertNoLeftovers(t *testing.T) {
	t.Helper()
	fs := p.fsHash()
	if fs == "" {
		// No Alphasfile written or unreadable — there's no run to
		// have left things over from. Caller decides whether that's
		// a bug; we silently no-op (the cleanup hook calls us
		// unconditionally and shouldn't blow up empty projects).
		return
	}

	entries, err := registry.ListByFsHash(p.home, fs)
	if err != nil {
		t.Errorf("AssertNoLeftovers: registry read failed: %v", err)
		return
	}
	if len(entries) > 0 {
		// Reap so the next test gets a clean registry, then report
		// what we cleaned up.
		_ = registry.ReapByFsHash(p.home, fs, 0, nil)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Service)
		}
		sort.Strings(names)
		t.Errorf("AssertNoLeftovers: registry still holds %d entries for fs_hash=%s (services: %s) — service didn't deregister on stop",
			len(entries), fs, strings.Join(names, ", "))
	}

	// Per-project state dir: socket file leftover suggests alpha
	// crashed without unlinking. Only check when WithExistingRoot
	// was used (in-place run); tmpdir projects get t.TempDir's
	// cleanup anyway.
	sock := findStaleSocket(p.root)
	if sock != "" {
		t.Errorf("AssertNoLeftovers: stale alpha socket at %s — alpha didn't shut down cleanly", sock)
	}
}

// fsHash resolves this project's identity the same way alpha does:
// from the invocation dir + Alphasfile bytes. Returns "" when no
// Alphasfile is present (test never wrote one), which is a signal
// to skip leftover checks rather than fail noisily.
func (p *Project) fsHash() string {
	af, err := os.ReadFile(filepath.Join(p.root, "Alphasfile"))
	if err != nil {
		return ""
	}
	inv, err := invocation.New(p.root, af, nil)
	if err != nil {
		return ""
	}
	return inv.FsHash
}

// findStaleSocket scans the project's per-run state for an alpha
// socket. Returns its path on hit, "" otherwise. Looks under
// <root>/.zordon/worktrees/* because that's where alpha stages its
// per-worktree state dir.
func findStaleSocket(root string) string {
	matches, _ := filepath.Glob(filepath.Join(root, ".zordon", "worktrees", "*", "alpha.sock"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}
