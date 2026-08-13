// Package zfs is the project's filesystem-safety chokepoint. It is
// not a re-export of "os"; it is a small set of use-case-named
// operations that bake in the right permissions, atomicity, and
// "what to do on absence" semantics so callers can't get them wrong.
//
// The rules zfs enforces (per AGENTS.md):
//
//   - Private state directories are always 0o750 (owner rwx, group
//     r-x, world none). zordon's state on disk — lock files, sockets,
//     registry, per-invocation tmp — never leaks readable to other
//     users on a shared host.
//   - Sensitive files (locks, logs, atomically-written state) are
//     always 0o600.
//   - Writes that must survive a crash mid-flight go via temp+rename
//     in the same directory, never an in-place truncate.
//
// What zfs is NOT: a re-implementation of "os". Use-case coverage is
// deliberate. Operations that have no convention angle (Stat,
// ReadFile) live here too, but only as thin chokepoints so business
// logic can drop its `import "os"` line — they're a small minority of
// the surface.
package zfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// File is a type alias so callers can hold a *zfs.File without
// importing "os". Methods come from *os.File.
type File = os.File

// dirPerm is the standard private-state directory mode used across
// zordon (lock dirs, state dirs, tmp dirs, registry parent, ...).
// Centralized so a future audit only has to touch this constant.
const dirPerm os.FileMode = 0o750

// filePerm is the standard sensitive-file mode (locks, logs, atomic
// writes). Owner-only.
const filePerm os.FileMode = 0o600

// PathListSeparator is the platform's PATH-style separator (':' on
// Unix, ';' on Windows). Exposed here so PATH-mangling code doesn't
// need `import "os"` for this one rune.
const PathListSeparator = os.PathListSeparator

// FileDescriptor is an inherited-fd flag type. It implements flag.Value
// so cmd/ binaries register it through ff once and get both `--ready-fd`
// CLI and `ZORDON_READY_FD` env binding for free — no scattered
// os.Getenv calls, no parsing logic at the call site.
//
// Semantics: 0 means "no fd inherited" (the documented "disabled"
// value); 1 and 2 are stdin/stdout, rejected because using them as
// extra parent-supervision fds collides with stdio; 3+ are valid (the
// first slot Go's exec.Cmd.ExtraFiles uses). Negative values are
// rejected.
type FileDescriptor int

// String renders the fd as a decimal — required by flag.Value so
// `--help` can print the default.
func (f *FileDescriptor) String() string {
	if f == nil {
		return "0"
	}
	return strconv.Itoa(int(*f))
}

// Set parses a string value (from CLI or env) and validates it.
func (f *FileDescriptor) Set(s string) error {
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("zfs: bad file descriptor %q: %w", s, err)
	}
	if n < 0 {
		return fmt.Errorf("zfs: file descriptor must be >= 0 (0=disabled, 3+=inherited), got %d", n)
	}
	if n == 1 || n == 2 {
		return fmt.Errorf("zfs: file descriptor %d collides with stdio; first inherited slot is 3", n)
	}
	*f = FileDescriptor(n)
	return nil
}

// Int returns the fd as an int for passing to syscall-shaped APIs.
func (f FileDescriptor) Int() int { return int(f) }

// IsSet reports whether a real fd was provided (non-zero).
func (f FileDescriptor) IsSet() bool { return f > 0 }

// DirName is a directory path. Implements flag.Value so cmd/ binaries
// register it once and get both `--home` CLI and ZORDON_HOME env
// binding through ff with no ad-hoc os.Getenv at the call site.
//
// Validation is intentionally minimal: an empty value is allowed and
// means "use the documented default at the consumer". Whitespace is
// trimmed. Path-existence and absoluteness checks are deferred to the
// consumer because the semantics differ per flag (some accept "" as
// "auto", others require a writable dir, etc.) — keeping that here
// would force one policy on everyone.
type DirName string

func (d *DirName) String() string {
	if d == nil {
		return ""
	}
	return string(*d)
}

func (d *DirName) Set(s string) error {
	*d = DirName(strings.TrimSpace(s))
	return nil
}

// String returns the path as a Go string for filepath.Join etc.
func (d DirName) Path() string { return string(d) }

// IsSet reports whether the user provided a non-empty value.
func (d DirName) IsSet() bool { return d != "" }

// EnsureDir makes path (and any missing parents) exist with the
// zordon-standard private mode (0o750). Use for: state dirs, lock
// dirs, registry parent, tmp dirs that hold sensitive files. The
// owner has rwx; the group has r-x; world has nothing.
//
// For directories that must be readable by other users on the host
// (per-invocation worktree checkouts, log dirs scrape-able by an
// admin), use EnsureSharedDir.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, dirPerm); err != nil {
		return fmt.Errorf("zfs: ensure dir %s: %w", path, err)
	}
	return nil
}

// sharedDirPerm is the world-readable directory mode (0o755).
// Reserved for dirs that legitimately need to be visible to other
// users on a shared host — worktree source checkouts (so `ls`-by-
// teammate works), $TMPDIR/zordon-* (other users can see the dir
// exists; the unix socket inside is still 0o600).
const sharedDirPerm os.FileMode = 0o755

// EnsureSharedDir makes path exist with 0o755 (world-readable).
// Use sparingly — the default and safer choice is EnsureDir. The
// legitimate use cases in zordon today are git worktree dirs and
// the per-invocation $TMPDIR/zordon-<hash> dir.
func EnsureSharedDir(path string) error {
	if err := os.MkdirAll(path, sharedDirPerm); err != nil {
		return fmt.Errorf("zfs: ensure shared dir %s: %w", path, err)
	}
	return nil
}

// AcquireLock takes a process-wide exclusive flock on
// <stateDir>/<name>.lock. The state dir is created if missing
// (EnsureDir semantics). The returned release function drops the
// lock and closes the FD; callers `defer release()`.
//
// This is the only way to take a file lock in zordon — control,
// registry, and tools all funnel through here so the lock file mode,
// open flags, and unlock-on-release behaviour stay uniform.
func AcquireLock(stateDir, name string) (release func(), err error) {
	if err := EnsureDir(stateDir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(stateDir, name+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("zfs: open lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("zfs: flock %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// AtomicWrite replaces path's contents with data via temp+sync+rename
// in the same directory. A crash mid-write can never leave a half-
// truncated file at path — only the previous good state, or the new
// good state. The parent directory must already exist (callers
// typically pair this with EnsureDir on the parent). Mode is 0o600.
func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("zfs: tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("zfs: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("zfs: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("zfs: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("zfs: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("zfs: rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// OpenAppendLog opens path in append-create mode with 0o600. Used
// for long-lived log files (alpha.log, level logs) where multiple
// writes from the same process are concatenated. Caller closes.
func OpenAppendLog(path string) (*File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return nil, fmt.Errorf("zfs: open append log %s: %w", path, err)
	}
	return f, nil
}

// RemoveIfPresent deletes a single file or empty dir at path. A
// missing path is success (no-op), not an error — most callers want
// "make sure this is gone" semantics on socket/tmp/marker cleanup.
// Use RemoveTree for recursive removal.
func RemoveIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("zfs: remove %s: %w", path, err)
	}
	return nil
}

// RemoveTree deletes path and everything under it. Missing path is
// success (mirrors os.RemoveAll). For wiping per-invocation state
// dirs, stale bare clones, dead worktrees.
func RemoveTree(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("zfs: remove tree %s: %w", path, err)
	}
	return nil
}

// Exists reports whether path exists (file or dir). Errors other
// than ErrNotExist are treated as "exists from the caller's point
// of view" — surfacing them via bool would lose information; if a
// caller needs to distinguish "missing" from "permission denied",
// they should call Stat directly.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// Missing is the inverse of Exists. Useful for readability:
// `if zfs.Missing(p) { ... }` reads better than `if !zfs.Exists(p)`.
func Missing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// IsExecutableFile reports whether path names a regular file (not a
// directory) with at least one execute bit set (owner, group, or
// other). Used by binary-resolution code that walks PATH or probes
// well-known install dirs for an existing tommy/mise/etc. False on
// any error (missing, permission denied) — callers continue the
// search instead of aborting.
func IsExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
}

// IsMissingErr reports whether err signals "no such file or
// directory" (or a wrapped equivalent). Use this instead of importing
// "os" for errors.Is(err, os.ErrNotExist) — same predicate, no
// `import "os"` in business logic.
func IsMissingErr(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// Stat is a thin chokepoint for stat(2). No convention to enforce;
// exposed so business logic doesn't import "os" just for one call.
// FileInfo is os.FileInfo (interface from io/fs).
func Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

// Read is a thin chokepoint for reading a whole file into memory.
// Same reason as Stat: avoid `import "os"` in business logic. Use
// for small config/manifest files; for large files stream via Open.
func Read(path string) ([]byte, error) { return os.ReadFile(path) }

// Open opens path for reading. Caller closes. Thin chokepoint.
func Open(path string) (*File, error) { return os.Open(path) }

// ReadDir returns the directory entries of name. Thin chokepoint;
// no convention to enforce. Use for listing worktrees, level logs.
func ReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }

// Getwd returns the current working dir. Thin chokepoint so cmd/
// can drop `import "os"` for this one call.
func Getwd() (string, error) { return os.Getwd() }

// Chdir changes the current working dir. Thin chokepoint, same reason
// as Getwd. Errors if dir does not exist or is not a directory.
func Chdir(dir string) error { return os.Chdir(dir) }

// SystemTempDir returns the OS-wide temp dir ($TMPDIR or /tmp). This
// is NOT the per-invocation tmp dir — that lives at
// <SystemTempDir>/zordon-<fsHash>/ and is computed at the call site.
// Exposed here so callers don't need `import "os"` for one call.
func SystemTempDir() string { return os.TempDir() }

// ZordonHome resolves the directory holding zordon's host-wide state
// (shared mise install, toolchain cache, port registry, bare-clone
// mirrors). Resolution order: explicit override wins; otherwise
// <UserHomeDir>/.zordon. Empty result only when UserHomeDir itself
// fails — callers degrade gracefully (no registry, no shared cache)
// rather than panicking.
//
// This is the ONLY function that resolves the home path. Per-binary
// `main()` calls it once at startup with the flag value; everything
// downstream takes the resolved string as a parameter. zordon's env-
// var bridge (ZORDON_HOME) is handled by the flag library (ff env-
// prefix), not by this function — keeps env reads out of business
// logic.
func ZordonHome(override string) string {
	if override != "" {
		return override
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".zordon")
}

// AtomicWriteKeepingMode is AtomicWrite that leaves an existing file's
// permissions alone. AtomicWrite always lands on filePerm, which is right for
// the private state zordon owns but wrong when it is editing a fragment of a
// file somebody else created: a 0644 config silently becoming 0600 can break
// whatever else reads it.
//
// A file that does not exist yet is created with AtomicWrite's own mode.
func AtomicWriteKeepingMode(path string, data []byte) error {
	mode := os.FileMode(0)
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		mode = fi.Mode().Perm()
	}
	if err := AtomicWrite(path, data); err != nil {
		return err
	}
	if mode == 0 || mode == filePerm {
		return nil
	}
	return os.Chmod(path, mode)
}

// Resolve joins a manifest-supplied relative path onto base and reports
// whether the result stays inside base. It is the containment primitive for
// every path that comes out of an Alphasfile: a generated file's target, a
// template's source. An absolute rel, a "../" climb and a symlink-free
// traversal all resolve to ok=false.
//
// base is assumed already trustworthy (an invocation dir, a project root).
// The returned path is cleaned and absolute whenever base is.
func Resolve(base, rel string) (path string, ok bool) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	base = filepath.Clean(base)
	path = filepath.Clean(filepath.Join(base, rel))
	return path, Within(base, path)
}

// Within reports whether path is base itself or lies beneath it. Both are
// cleaned first; neither is required to exist.
func Within(base, path string) bool {
	base, path = filepath.Clean(base), filepath.Clean(path)
	if path == base {
		return true
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ServiceCwd is the canonical "where does this service work from?"
// answer: the checkout root joined with the service's exe offset.
// Single source of truth across the codebase — the resolved value is
// baked into Service.Runtime.Dir at eval time, exposed to HCL as
// `fs::src()`, and read by alpha for build cwd, runtime cwd, and
// provision cwd. Adding a new consumer means calling THIS function,
// not re-deriving the join inline.
//
// `exe` semantics:
//
//   - "" / "."         ⇒ the checkout itself (no offset).
//   - relative path    ⇒ joined to checkout; the result is the dir
//     where the user holds go.mod / Cargo.toml /
//     package.json.
//   - absolute path    ⇒ returned verbatim (escape hatch; not used
//     by current HCL grammar but kept robust).
//
// Empty `checkout` returns "" — callers (use-only crate installs)
// fall back to their own cwd policy.
func ServiceCwd(checkout, exe string) string {
	if checkout == "" {
		return ""
	}
	exe = strings.TrimSpace(exe)
	if exe == "" || exe == "." {
		return checkout
	}
	if filepath.IsAbs(exe) {
		return filepath.Clean(exe)
	}
	return filepath.Clean(filepath.Join(checkout, exe))
}

// CheckSpawnCwd validates a resolved service working directory (the output
// of ServiceCwd) before it is handed to os/exec as cmd.Dir. It is the
// spawn-time companion to the eval-time ServiceCwd builder.
//
// Why it exists: os/exec reports a missing cwd as a fork/exec PathError
// against cmd.Path — the toolchain binary — not against the directory. So a
// build/runtime spawned with a vanished checkout subdir surfaces as
// "fork/exec <…>/go: no such file or directory", pointing at a healthy
// binary. This check turns that red herring into the real cause.
//
// dir "" is a no-op (use-only installs leave cmd.Dir unset). exe is the
// src{} exe offset (may be "") used only to enrich the message.
func CheckSpawnCwd(dir, exe string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("working directory is not a directory: %s", dir)
	case !IsMissingErr(err):
		return err // permission-denied etc. — surface verbatim
	}
	if exe = strings.TrimSpace(exe); exe != "" && exe != "." {
		return fmt.Errorf("working directory does not exist: %s (src exe %q not found in checkout — removed upstream?)", dir, exe)
	}
	return fmt.Errorf("working directory does not exist: %s", dir)
}

// Resolver computes the on-disk paths that depend on a single zordon
// run's context — the directory it was invoked from and that run's
// FsHash. It is the intended single home for every derived path in
// zordon: as call sites stop hand-rolling filepath.Join's, their
// methods move here so exactly one place knows the layout.
//
// Either field may be "" when the caller doesn't have it (alpha, for
// instance, recovers only the hash from its socket path). Each method
// documents which fields it relies on.
//
// A Resolver narrows by refinement: ForToolchain returns a clone carrying
// extra scope (the toolchain's resolved env), and scope-specific queries
// (ToolBinDirs) read that state instead of taking it as arguments. Because
// every field is a value type, the value receiver IS the clone — refinement
// needs no copy machinery.
type Resolver struct {
	dir  string
	hash string
	// toolchain scope, set by ForToolchain; tcScoped guards the queries
	// that require it. Empty/false on an unrefined Resolver.
	tcScoped       bool
	tcResolvedPATH string
	tcBasePATH     string
}

// NewResolver builds a Resolver for the run rooted at dir with FsHash
// hash.
func NewResolver(dir, hash string) Resolver { return Resolver{dir: dir, hash: hash} }

// ForToolchain returns a clone scoped to a toolchain whose `mise env`
// produced resolvedPATH, layered over basePATH (the PATH handed to mise).
// The clone is what ToolBinDirs reads. Value receiver → the original is
// untouched and no deep-copy is needed (every field is a value type).
func (r Resolver) ForToolchain(resolvedPATH, basePATH string) Resolver {
	r.tcScoped = true
	r.tcResolvedPATH = resolvedPATH
	r.tcBasePATH = basePATH
	return r
}

// AlphaLogFile resolves where a leaf alpha logs. A non-empty override
// (the operator's explicit path) is returned unchanged; otherwise it
// falls back to a stable, easy-to-tail file in the system temp dir,
// namespaced by FsHash so runs from different workspaces never clobber
// one shared alpha.log. The fallback relies on hash.
func (r Resolver) AlphaLogFile(override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(SystemTempDir(), "alpha-"+r.hash+".log")
}

// ToolBinDirs returns the bin dir(s) the scoped toolchain contributes: the
// leading entries of its resolved PATH not present in the base PATH,
// stopping at the first entry that is. `mise env` prepends a tool's bin
// dir(s) to the PATH it is handed, so the genuinely-new dirs are a
// contiguous leading prefix; a shared dir already in the base (e.g. the
// zordon mise bin dir) is therefore excluded.
//
// Panics if the resolver isn't toolchain-scoped: calling it without
// ForToolchain is API misuse (a programmer error), not a runtime condition
// (AGENTS.md). The scope rather than arguments is what makes the two PATHs
// impossible to swap at the call site.
func (r Resolver) ToolBinDirs() []string {
	if !r.tcScoped {
		panic("zfs: Resolver.ToolBinDirs requires a toolchain scope — call ForToolchain first")
	}
	if r.tcResolvedPATH == "" {
		return nil
	}
	baseSet := make(map[string]struct{})
	for _, d := range filepath.SplitList(r.tcBasePATH) {
		if d != "" {
			baseSet[d] = struct{}{}
		}
	}
	var out []string
	for _, d := range filepath.SplitList(r.tcResolvedPATH) {
		if d == "" {
			continue
		}
		if _, ok := baseSet[d]; ok {
			break
		}
		out = append(out, d)
	}
	return out
}

// Copy streams src into dst with 0o600. Used for moving build
// artifacts into invocation bin dirs without going through `cp` and
// without inheriting an in-tree mode.
func Copy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("zfs: open src %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("zfs: open dst %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("zfs: copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("zfs: close %s: %w", dst, err)
	}
	return nil
}
