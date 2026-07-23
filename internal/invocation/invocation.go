// Package invocation is the single source of truth for "which run of zordon
// is this, and where does its state live". It is a leaf package (stdlib
// only) so both the CLI and the resolver can depend on it without cycles.
//
// Every run happens in some workspace — the project root is just the
// implicit workspace named "main". A run from
// <X>/workspaces/<name>/ is the workspace "<name>" over the same
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

const MainWorkspace = "main"

// WorkspaceMarker is the empty file `zordon workspace create` drops into a
// workspace dir as an explicit, durable "this is a workspace" signal (also
// read by agents via SKILL.md). The <root>/workspaces/<name>/ path stays
// authoritative, so removing the marker can't un-workspace one.
const WorkspaceMarker = ".workspace"

// WorkspaceName is the identifier of a workspace — "main" for the
// implicit project-root workspace, or a user-chosen label for a side
// checkout under <root>/workspaces/<name>. Implements
// flag.Value so cmd/alpha and cmd/zordon register it directly as a
// flag (auto-mapped to ZORDON_WORKSPACE).
//
// Validation: empty value normalizes to MainWorkspace (so callers can
// always use the value as a non-empty path component); otherwise
// allows letters, digits, '-', '_'. Anything else is rejected because
// the name flows into git branch refs and filesystem paths where '/'
// or shell metacharacters would surprise users.
type WorkspaceName string

func (w *WorkspaceName) String() string {
	if w == nil {
		return MainWorkspace
	}
	if *w == "" {
		return MainWorkspace
	}
	return string(*w)
}

func (w *WorkspaceName) Set(s string) error {
	if s == "" {
		*w = MainWorkspace
		return nil
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("invocation: workspace name %q: only [A-Za-z0-9_-] allowed", s)
		}
	}
	*w = WorkspaceName(s)
	return nil
}

// Name returns the workspace label as a plain string (== MainWorkspace
// for the zero value), suitable for path joining and branch refs.
func (w WorkspaceName) Name() string {
	if w == "" {
		return MainWorkspace
	}
	return string(w)
}

// IsMain reports whether this is the implicit project-root workspace.
func (w WorkspaceName) IsMain() bool {
	return w == "" || w == MainWorkspace
}

// InvocationState is the filesystem-location identity of a run: pure directory
// facts. FsHash depends only on Dir, so two runs from the same directory get
// the same FsHash, letting zordon re-attach to an already-running alpha; it
// names the socket / tmp dir and answers "is this the same alpha instance?".
//
// The manifest identity (ConfigHash) is deliberately NOT here — it depends on the
// Alphasfile bytes + resolved parent context, so it is computed at the call
// site via ConfigHash() and carried by the resolved alphasfile.Alphasfile.
type InvocationState struct {
	Dir       string            // normalized invocation dir (project root or workspace dir)
	Workspace string            // "main" or "<name>"
	StateDir  string            // <X>/workspaces/<Workspace>
	FsHash    string            // sha8(Dir) — instance identity
	TmpDir    string            // $TMPDIR/zordon-<FsHash>  (short; sockets live here)
	Env       map[string]string // injected into spawned services

	// OwnedServices is the set of services this (named) workspace took at
	// `zordon workspace create <ws> [svc ...]`. Populated by build() from a
	// scan of <StateDir>/src/*/.git (presence of .git = a real `git worktree
	// add` was performed). Services NOT in this set fall back to the anchor
	// Alphasfile's resolved dir (same as the main workspace's InPlace) so
	// they share the user's live tree instead of getting a forked, empty
	// per-workspace checkout.
	//
	// nil (the zero value) is "ownership unknown" — preserved for legacy
	// struct-literal callers and historical behavior (treat every service
	// as owned). Never consulted in the main workspace.
	OwnedServices map[string]bool
}

// OwnsService reports whether the named workspace carries its own per-service
// checkout for svc. Always false in the main workspace (main never forks; it
// uses the live src tree in place). In named workspaces: nil OwnedServices ⇒
// legacy "all owned" default (struct-literal callers in tests); non-nil ⇒
// explicit set membership (the production path, populated from disk in
// build()). The MainWorkspace check uses the literal "main" string only —
// an empty Workspace is treated as a (degenerate) named workspace, matching
// the historical eval semantics relied on by oracle tests.
func (i *InvocationState) OwnsService(svc string) bool {
	if i == nil || i.Workspace == MainWorkspace {
		return false
	}
	if i.OwnedServices == nil {
		return true
	}
	return i.OwnedServices[svc]
}

// CheckoutPath is where service <svc>'s git worktree is materialized for
// this invocation.
func (i *InvocationState) CheckoutPath(svc string) string {
	return filepath.Join(i.StateDir, "src", svc)
}

// BinDir is where build outputs land for this invocation — deliberately
// OUTSIDE the source checkouts so building never dirties a `dir` primary's
// git worktree. This is what fs::bin() returns.
func (i *InvocationState) BinDir() string {
	return filepath.Join(i.StateDir, "bin")
}

// ProjectRoot is the directory the leaf Alphasfile lives in (the workspace's
// <X>). Relative `dir` primaries resolve against this.
func (i *InvocationState) ProjectRoot() string {
	// StateDir == <root>/workspaces/<ws>
	return filepath.Dir(filepath.Dir(i.StateDir))
}

// SocketPath is the alpha control socket for this invocation (kept under
// $TMPDIR to stay within the unix-socket path length limit).
func (i *InvocationState) SocketPath() string {
	return filepath.Join(i.TmpDir, "alpha.sock")
}

// LockPath / AlphaLogPath live in the (in-tree) state dir.
func (i *InvocationState) LockPath() string     { return filepath.Join(i.StateDir, "start.lock") }
func (i *InvocationState) AlphaLogPath() string { return filepath.Join(i.StateDir, "alpha.log") }

// projectRootAndWorkspace decides, purely from a directory, whether it is a
// workspace dir (<X>/workspaces/<name>) and returns the project root
// <X> plus the workspace name. Otherwise the dir itself is the project root
// and the workspace is "main".
func projectRootAndWorkspace(dir string) (root, workspace string) {
	clean := filepath.Clean(dir)
	parent := filepath.Dir(clean) // .../workspaces
	// <root>/workspaces/<name>/. The path is authoritative; the .workspace
	// marker create drops there is an equivalent positive signal, so a
	// conventional workspace stays one even if the marker is removed.
	if filepath.Base(parent) == "workspaces" || zfs.Exists(filepath.Join(clean, WorkspaceMarker)) {
		return filepath.Dir(parent), filepath.Base(clean)
	}
	return clean, MainWorkspace
}

// Resolve reports the project root and workspace name a directory belongs to.
// It is the exported form of the rule NewInvocationState applies, so callers
// that must decide "is this a legal place to run from" (the CLI's invocation
// gate) share one definition with the state builder instead of re-deriving it.
func Resolve(dir string) (root, workspace string) {
	return projectRootAndWorkspace(dir)
}

// EnclosingCheckout reports whether dir lies at or under a zordon-managed
// per-service checkout — <outerRoot>/workspaces/<ws>/src/<svc> — and returns
// the enclosing project root plus the workspace and service that own it.
//
// The answer is derived from path shape (plus the .workspace marker for
// workspaces that live outside the conventional <root>/workspaces/ dir), NOT
// from the presence of a manifest: this package deliberately knows nothing
// about the Alphasfile's name. Callers that need "…and the outer project is
// real" check for the manifest themselves.
func EnclosingCheckout(dir string) (outerRoot, workspace, service string, ok bool) {
	d := filepath.Clean(dir)
	for {
		if filepath.Base(filepath.Dir(d)) == "src" {
			svcDir := d
			wsDir := filepath.Dir(filepath.Dir(svcDir))
			// outerRoot mirrors projectRootAndWorkspace(wsDir): the root is the
			// grandparent of the workspace dir in BOTH layouts — the
			// conventional <root>/workspaces/<ws> and a .workspace-marked dir
			// two levels below its root. Only the trigger differs.
			if filepath.Base(filepath.Dir(wsDir)) == "workspaces" ||
				zfs.Exists(filepath.Join(wsDir, WorkspaceMarker)) {
				return filepath.Dir(filepath.Dir(wsDir)), filepath.Base(wsDir), filepath.Base(svcDir), true
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", "", false
		}
		d = parent
	}
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

// NewInvocationState builds the invocation for a leaf Alphasfile. invocationDir is the
// directory the user ran zordon from (normalized CWD). Pure directory facts:
// the manifest identity (ConfigHash) is NOT here — it is the resolved
// Alphasfile's concern (see ConfigHash + alphasfile.Alphasfile.CfgHash).
func NewInvocationState(invocationDir string) (*InvocationState, error) {
	abs, err := filepath.Abs(invocationDir)
	if err != nil {
		return nil, err
	}
	root, ws := projectRootAndWorkspace(abs)
	return build(abs, root, ws), nil
}

// NewInvocationStateAt builds the invocation for an Alphasfile addressed by its own
// directory (used for federation parents, which are never workspaces —
// always "main" rooted at the file's dir).
func NewInvocationStateAt(alphasfileDir string) (*InvocationState, error) {
	abs, err := filepath.Abs(alphasfileDir)
	if err != nil {
		return nil, err
	}
	return build(abs, abs, MainWorkspace), nil
}

// ConfigHash is the manifest identity: sha8 of the Alphasfile bytes joined with
// the serialized resolved parent context. It changes whenever the manifest (or
// any federation parent it sees) changes, and drives drift detection. It lives
// here (next to FsHash) because both share shortSum, but it is a property of
// the (bytes, parent) pair — NOT of the directory — so it is computed at the
// call site, never stored on InvocationState.
func ConfigHash(afBytes, parentCtx []byte) string {
	return shortSum(afBytes, parentCtx)
}

func build(dir, root, ws string) *InvocationState {
	stateDir := filepath.Join(root, "workspaces", ws)
	fsh := shortSum([]byte(dir))
	tmp := filepath.Join(zfs.SystemTempDir(), "zordon-"+fsh)
	return &InvocationState{
		Dir:           dir,
		Workspace:     ws,
		StateDir:      stateDir,
		FsHash:        fsh,
		TmpDir:        tmp,
		OwnedServices: scanOwnedServices(stateDir, ws),
		Env: map[string]string{
			"ZORDON_WORKSPACE": ws,
			"ZORDON_STATE_DIR": stateDir,
			"ZORDON_FS_HASH":   fsh,
		},
	}
}

// scanOwnedServices reads <stateDir>/src/*/.git markers (one per service
// that got a real `git worktree add`). Returns nil for the main workspace
// (irrelevant — main never forks) and for a fresh workspace with no
// materialized services (== "ownership unknown", legacy default).
func scanOwnedServices(stateDir, ws string) map[string]bool {
	if ws == "" || ws == MainWorkspace {
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
