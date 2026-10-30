package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
)

// SplitIdentity splits a Go-style identity such as
// github.com/owner/repo/stacks/kafka@v1.2.0 into the repository
// (github.com/owner/repo), the path inside it (stacks/kafka) and the version
// (v1.2.0). The version is empty when none is given.
func SplitIdentity(id string) (repo, sub, ref string, err error) {
	path, ref, hasRef := strings.Cut(id, "@")
	if hasRef && ref == "" {
		return "", "", "", fmt.Errorf("%q: empty version after @", id)
	}
	path = strings.Trim(path, "/")
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return "", "", "", fmt.Errorf("%q: a path segment cannot be empty, . or .., so an identity always stays inside its repository", id)
		}
	}
	if hasRef {
		if err := CheckRef(ref); err != nil {
			return "", "", "", fmt.Errorf("%q: %w", id, err)
		}
	}
	repo, err = normalizeGit(path)
	if err != nil {
		return "", "", "", err
	}
	sub = strings.TrimPrefix(strings.TrimPrefix(path, repo), "/")
	return repo, sub, ref, nil
}

// CheckRef rejects a ref git could read as an option or that no branch, tag
// or commit can be named: one starting with - or holding whitespace or
// control characters.
func CheckRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.IndexFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return fmt.Errorf("ref names a branch, tag or commit; it cannot start with - or hold spaces, got %q", ref)
	}
	return nil
}

// IsCommit reports whether s is a full sha-1 or sha-256 commit hash.
func IsCommit(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') }) < 0
}

// ResolveCommit resolves a branch, tag or commit to a full commit sha in the
// primary's git dir: the bare clone for a git primary (call Ensure first so
// it has the ref), the repository itself for a dir primary.
func (p Primary) ResolveCommit(ctx context.Context, ref string) (string, error) {
	if !p.Workspaceable() {
		return "", errors.New("source.ResolveCommit: service has no git/dir primary")
	}
	var out bytes.Buffer
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", p.primaryPath(), "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s has no branch, tag or commit %q", p.Repo, ref)
	}
	return strings.TrimSpace(out.String()), nil
}

// ModFetcher fetches repositories that Alphasfiles import by identity. It
// keeps the bare clones under the zordon home, like service sources.
type ModFetcher struct {
	Home     string
	Keychain zlock.GlobalKeychain
}

// Resolve fetches repo and returns the commit ref points at now.
func (f ModFetcher) Resolve(repo, ref string) (string, error) {
	release, err := f.Keychain.LockRepo(context.Background(), repo)
	if err != nil {
		return "", err
	}
	defer release()
	ctx := context.Background()
	p, err := NewPrimary(f.Home, repo, "", ref, nil)
	if err != nil {
		return "", err
	}
	if err := p.Ensure(ctx, captureRunner); err != nil {
		return "", fmt.Errorf("fetch %s: %w", repo, err)
	}
	return p.ResolveCommit(ctx, ref)
}

// Materialize checks repo out at commit into dest. A finished checkout of the
// same commit is reused without touching the network.
func (f ModFetcher) Materialize(repo, commit, dest string) error {
	if !IsCommit(commit) {
		return fmt.Errorf("check out %s: %q is not a commit hash", repo, commit)
	}
	release, err := f.Keychain.LockRepo(context.Background(), repo)
	if err != nil {
		return err
	}
	defer release()
	p, err := NewPrimary(f.Home, repo, "", commit, nil)
	if err != nil {
		return err
	}
	if err := p.Clone(context.Background(), dest, commit, captureRunner); err != nil {
		return fmt.Errorf("check out %s@%s: %w", repo, commit, err)
	}
	return nil
}

// Behind counts the commits ref has that commit does not in repo's bare
// clone, without the network: as of the last fetch, whose time it returns. A
// repository never fetched, or one that has not fetched commit yet, yields an
// error wrapping fs.ErrNotExist.
func (f ModFetcher) Behind(ctx context.Context, repo, ref, commit string) (int, time.Time, error) {
	if !IsCommit(commit) {
		return 0, time.Time{}, fmt.Errorf("count %s behind %s: %q is not a commit hash", repo, ref, commit)
	}
	p, err := NewPrimary(f.Home, repo, "", ref, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	bare := p.primaryPath()
	fetched, err := lastFetch(bare)
	if err != nil {
		return 0, time.Time{}, err
	}
	release, err := f.Keychain.LockRepo(ctx, repo)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer release()
	n, err := countBehind(ctx, bare, ref, commit)
	return n, fetched, err
}

// captureRunner runs git quietly and folds its output into the error, so a
// failed fetch explains itself without streaming progress into zordon's
// output.
func captureRunner(_ context.Context, cmd *exec.Cmd) error {
	if cmd.Env == nil {
		cmd.Env = append(zenv.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// countBehind counts the commits ref has that commit does not, in gitDir. A
// checkout is cloned from upstream, not from gitDir, so gitDir may lack a
// commit the lock holds: one a teammate pinned since its last fetch.
func countBehind(ctx context.Context, gitDir, ref, commit string) (int, error) {
	if err := CheckRef(ref); err != nil {
		return 0, err
	}
	if err := exec.CommandContext(ctx, "git", "-C", gitDir, "cat-file", "-e", commit+"^{commit}").Run(); err != nil {
		return 0, fmt.Errorf("%s has not fetched %s yet: %w", gitDir, shortRef(commit), fs.ErrNotExist)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-list", "--count", commit+".."+ref).Output()
	if err != nil {
		return 0, fmt.Errorf("count %s..%s in %s: %w", shortRef(commit), ref, gitDir, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// lastFetch is when a bare clone last talked to its remote: FETCH_HEAD after
// a fetch, packed-refs after the clone itself.
func lastFetch(bare string) (time.Time, error) {
	for _, name := range []string{"FETCH_HEAD", "packed-refs"} {
		if fi, err := zfs.Stat(filepath.Join(bare, name)); err == nil {
			return fi.ModTime(), nil
		}
	}
	if _, err := zfs.Stat(bare); err != nil {
		return time.Time{}, fmt.Errorf("%s not fetched yet: %w", bare, err)
	}
	return time.Time{}, fmt.Errorf("%s has no fetch to date: %w", bare, fs.ErrNotExist)
}

func shortRef(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
