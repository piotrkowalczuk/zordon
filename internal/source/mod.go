package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zenv"
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
	repo, err = normalizeGit(path)
	if err != nil {
		return "", "", "", err
	}
	sub = strings.TrimPrefix(strings.TrimPrefix(path, repo), "/")
	return repo, sub, ref, nil
}

// ResolveCommit resolves a branch, tag or commit to a full commit sha in the
// primary's git dir: the bare clone for a git primary (call Ensure first so
// it has the ref), the repository itself for a dir primary.
func (p Primary) ResolveCommit(ctx context.Context, ref string) (string, error) {
	if !p.Workspaceable() {
		return "", errors.New("source.ResolveCommit: service has no git/dir primary")
	}
	var out bytes.Buffer
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
	Home string
}

// Resolve fetches repo and returns the commit ref points at now.
func (f ModFetcher) Resolve(repo, ref string) (string, error) {
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
	p, err := NewPrimary(f.Home, repo, "", commit, nil)
	if err != nil {
		return err
	}
	if err := p.Clone(context.Background(), dest, commit, captureRunner); err != nil {
		return fmt.Errorf("check out %s@%s: %w", repo, commit, err)
	}
	return nil
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
