// Package invocation is the single source of truth for "which run of zordon
// is this, and where does its state live". It is a leaf package (stdlib
// only) so both the CLI and the resolver can depend on it without cycles.
//
// Every run happens in some worktree — the project root is just the
// implicit worktree named "main". A run from
// <X>/.zordon/worktrees/<name>/ is the worktree "<name>" over the same
// adopted Alphasfile (<X>/Alphasfile), with its own state dir, hash, tmp
// dir and per-service checkouts. There is no separate "default mode": the
// only thing that differs is the name.
package invocation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

const MainWorktree = "main"

// WorktreeName is the identifier of a worktree — "main" for the
// implicit project-root worktree, or a user-chosen label for a side
// checkout under <root>/.zordon/worktrees/<name>. Implements
// flag.Value so cmd/alpha and cmd/zordon register it directly as a
// flag (auto-mapped to ZORDON_WORKTREE).
//
// Validation: empty value normalizes to MainWorktree (so callers can
// always use the value as a non-empty path component); otherwise
// allows letters, digits, '-', '_'. Anything else is rejected because
// the name flows into git branch refs and filesystem paths where '/'
// or shell metacharacters would surprise users.
type WorktreeName string

func (w *WorktreeName) String() string {
	if w == nil {
		return MainWorktree
	}
	if *w == "" {
		return MainWorktree
	}
	return string(*w)
}

func (w *WorktreeName) Set(s string) error {
	if s == "" {
		*w = MainWorktree
		return nil
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("invocation: worktree name %q: only [A-Za-z0-9_-] allowed", s)
		}
	}
	*w = WorktreeName(s)
	return nil
}

// Name returns the worktree label as a plain string (== MainWorktree
// for the zero value), suitable for path joining and branch refs.
func (w WorktreeName) Name() string {
	if w == "" {
		return MainWorktree
	}
	return string(w)
}

// IsMain reports whether this is the implicit project-root worktree.
func (w WorktreeName) IsMain() bool {
	return w == "" || w == MainWorktree
}

// Invocation carries two orthogonal identities:
//
//   - FsHash is the filesystem-location identity. It depends only on Dir, so
//     two runs from the same directory get the same FsHash, allowing zordon
//     to re-attach to an already-running alpha. It names the socket / tmp
//     dir and answers "is this the same alpha instance?".
//
//   - CfgHash is the manifest identity. It depends on the Alphasfile bytes
//     and the resolved parent context, so it changes whenever the manifest
//     (or any federation parent it sees) changes. It answers "is the
//     configuration of this alpha still current?" and drives drift
//     detection in federation chains.
type Invocation struct {
	Dir      string            // normalized invocation dir (project root or worktree dir)
	Worktree string            // "main" or "<name>"
	StateDir string            // <X>/.zordon/worktrees/<Worktree>
	FsHash   string            // sha8(Dir)               — instance identity
	CfgHash  string            // sha8(afBytes+parentCtx) — manifest identity
	TmpDir   string            // $TMPDIR/zordon-<FsHash>  (short; sockets live here)
	Env      map[string]string // injected into spawned services

	// OwnedServices is the set of services this (named) worktree took at
	// `zordon worktree create <wt> [svc ...]`. Populated by build() from a
	// scan of <StateDir>/src/*/.git (presence of .git = a real `git worktree
	// add` was performed). Services NOT in this set fall back to the anchor
	// Alphasfile's resolved dir (same as the main worktree's InPlace) so
	// they share the user's live tree instead of getting a forked, empty
	// per-worktree checkout.
	//
	// nil (the zero value) is "ownership unknown" — preserved for legacy
	// struct-literal callers and historical behavior (treat every service
	// as owned). Never consulted in the main worktree.
	OwnedServices map[string]bool
}

// OwnsService reports whether the named worktree carries its own per-service
// checkout for svc. Always false in the main worktree (main never forks; it
// uses the live src tree in place). In named worktrees: nil OwnedServices ⇒
// legacy "all owned" default (struct-literal callers in tests); non-nil ⇒
// explicit set membership (the production path, populated from disk in
// build()). The MainWorktree check uses the literal "main" string only —
// an empty Worktree is treated as a (degenerate) named worktree, matching
// the historical eval semantics relied on by oracle tests.
func (i *Invocation) OwnsService(svc string) bool {
	if i == nil || i.Worktree == MainWorktree {
		return false
	}
	if i.OwnedServices == nil {
		return true
	}
	return i.OwnedServices[svc]
}

// CheckoutPath is where service <svc>'s git worktree is materialized for
// this invocation.
func (i *Invocation) CheckoutPath(svc string) string {
	return filepath.Join(i.StateDir, "src", svc)
}

// BinDir is where build outputs land for this invocation — deliberately
// OUTSIDE the source checkouts so building never dirties a `dir` primary's
// git worktree. This is what fs::bin() returns.
func (i *Invocation) BinDir() string {
	return filepath.Join(i.StateDir, "bin")
}

// ProjectRoot is the directory the leaf Alphasfile lives in (the worktree's
// <X>). Relative `dir` primaries resolve against this.
func (i *Invocation) ProjectRoot() string {
	// StateDir == <root>/.zordon/worktrees/<wt>
	return filepath.Dir(filepath.Dir(filepath.Dir(i.StateDir)))
}

// SocketPath is the alpha control socket for this invocation (kept under
// $TMPDIR to stay within the unix-socket path length limit).
func (i *Invocation) SocketPath() string {
	return filepath.Join(i.TmpDir, "alpha.sock")
}

// LockPath / AlphaLogPath live in the (in-tree) state dir.
func (i *Invocation) LockPath() string     { return filepath.Join(i.StateDir, "start.lock") }
func (i *Invocation) AlphaLogPath() string { return filepath.Join(i.StateDir, "alpha.log") }

// projectRootAndWorktree decides, purely from a directory, whether it is a
// worktree dir (<X>/.zordon/worktrees/<name>) and returns the project root
// <X> plus the worktree name. Otherwise the dir itself is the project root
// and the worktree is "main".
func projectRootAndWorktree(dir string) (root, worktree string) {
	clean := filepath.Clean(dir)
	parent := filepath.Dir(clean)             // .../.zordon/worktrees
	gp := filepath.Dir(parent)                // .../.zordon
	if filepath.Base(parent) == "worktrees" && filepath.Base(gp) == ".zordon" {
		return filepath.Dir(gp), filepath.Base(clean)
	}
	return clean, MainWorktree
}

// shortSum returns the first 16 hex chars of sha256(parts joined by 0-byte).
// Joining with NUL keeps boundaries unambiguous so two distinct inputs can
// never collide by concatenation.
func shortSum(parts ...[]byte) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// New builds the invocation for a leaf Alphasfile. invocationDir is the
// directory the user ran zordon from (normalized CWD); afBytes is the
// adopted Alphasfile's content; parentCtx is the serialized resolved
// services of all federation parents (nil for a standalone file).
func New(invocationDir string, afBytes, parentCtx []byte) (*Invocation, error) {
	abs, err := filepath.Abs(invocationDir)
	if err != nil {
		return nil, err
	}
	root, wt := projectRootAndWorktree(abs)
	return build(abs, root, wt, afBytes, parentCtx)
}

// NewAt builds the invocation for an Alphasfile addressed by its own
// directory (used for federation parents, which are never worktrees —
// always "main" rooted at the file's dir).
func NewAt(alphasfileDir string, afBytes, parentCtx []byte) (*Invocation, error) {
	abs, err := filepath.Abs(alphasfileDir)
	if err != nil {
		return nil, err
	}
	return build(abs, abs, MainWorktree, afBytes, parentCtx)
}

func build(dir, root, wt string, afBytes, parentCtx []byte) (*Invocation, error) {
	stateDir := filepath.Join(root, ".zordon", "worktrees", wt)
	fsh := shortSum([]byte(dir))
	cfgh := shortSum(afBytes, parentCtx)
	tmp := filepath.Join(zfs.SystemTempDir(), "zordon-"+fsh)
	return &Invocation{
		Dir:           dir,
		Worktree:      wt,
		StateDir:      stateDir,
		FsHash:        fsh,
		CfgHash:       cfgh,
		TmpDir:        tmp,
		OwnedServices: scanOwnedServices(stateDir, wt),
		Env: map[string]string{
			"ZORDON_WORKTREE":  wt,
			"ZORDON_STATE_DIR": stateDir,
			"ZORDON_FS_HASH":   fsh,
			"ZORDON_CFG_HASH":  cfgh,
		},
	}, nil
}

// scanOwnedServices reads <stateDir>/src/*/.git markers (one per service
// that got a real `git worktree add`). Returns nil for the main worktree
// (irrelevant — main never forks) and for a fresh worktree with no
// materialized services (== "ownership unknown", legacy default).
func scanOwnedServices(stateDir, wt string) map[string]bool {
	if wt == "" || wt == MainWorktree {
		return nil
	}
	entries, err := zfs.ReadDir(filepath.Join(stateDir, "src"))
	if err != nil {
		return nil
	}
	owned := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := zfs.Stat(filepath.Join(stateDir, "src", e.Name(), ".git")); err == nil {
			owned[e.Name()] = true
		}
	}
	if len(owned) == 0 {
		return nil
	}
	return owned
}

// IsWorktreeDir reports whether dir is inside a .zordon/worktrees/<name>
// layout (used by walk-up to adopt the parent Alphasfile as the leaf).
func IsWorktreeDir(dir string) bool {
	_, wt := projectRootAndWorktree(dir)
	return wt != MainWorktree
}
