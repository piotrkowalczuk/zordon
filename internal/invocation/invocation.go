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
	"os"
	"path/filepath"
)

const MainWorktree = "main"

type Invocation struct {
	Dir      string            // normalized invocation dir (project root or worktree dir)
	Worktree string            // "main" or "<name>"
	StateDir string            // <X>/.zordon/worktrees/<Worktree>
	Hash     string            // sha8(Dir + alphasfile bytes + parent ctx)
	TmpDir   string            // $TMPDIR/zordon-<Hash>  (short; sockets live here)
	Env      map[string]string // injected into spawned services
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

// hash binds the invocation dir, the Alphasfile bytes, and the parent
// federation context into one short id. Two runs of the same file from
// different invocation dirs (e.g. main vs a worktree) get distinct hashes
// ⇒ disjoint state, sockets, and pickport() draws.
func hash(dir string, alphasfile, parentCtx []byte) string {
	h := sha256.New()
	h.Write([]byte(dir))
	h.Write([]byte{0})
	h.Write(alphasfile)
	h.Write([]byte{0})
	h.Write(parentCtx)
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
	h := hash(dir, afBytes, parentCtx)
	tmp := filepath.Join(os.TempDir(), "zordon-"+h)
	return &Invocation{
		Dir:      dir,
		Worktree: wt,
		StateDir: stateDir,
		Hash:     h,
		TmpDir:   tmp,
		Env: map[string]string{
			"ZORDON_WORKTREE":  wt,
			"ZORDON_STATE_DIR": stateDir,
			"ZORDON_HASH":      h,
		},
	}, nil
}

// IsWorktreeDir reports whether dir is inside a .zordon/worktrees/<name>
// layout (used by walk-up to adopt the parent Alphasfile as the leaf).
func IsWorktreeDir(dir string) bool {
	_, wt := projectRootAndWorktree(dir)
	return wt != MainWorktree
}
