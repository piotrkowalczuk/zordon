// Package source resolves and materializes service source trees under
// $HOME/.zordon/src. It owns the "where does the code live on disk"
// question for any service that needs to be built or run from a checkout
// (Go import paths, Rust git deps, Ruby git sources).
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Info points at a resolved checkout: the URL we clone from, the directory
// it lives in on disk, and the subpath inside the repo (for Go import paths
// that include a directory below the repo root, e.g. .../cmd/foo).
type Info struct {
	RepoURL string
	RepoDir string
	Subpath string
}

// Home returns the on-disk root for zordon-managed source checkouts.
// Falls back to "./.zordon" if $HOME cannot be resolved.
func Home() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".zordon")
	}
	return ".zordon"
}

// Resolve splits a source string (Go-style import path OR a host/owner/repo
// triple from a git URL we already trimmed) into RepoURL/RepoDir/Subpath.
//
// Only github.com, gitlab.com, and bitbucket.org are supported — anything
// else would need vanity-import resolution, which we don't do.
func Resolve(src string) (Info, error) {
	src = strings.TrimPrefix(src, "https://")
	src = strings.TrimPrefix(src, "http://")
	src = strings.TrimSuffix(src, ".git")
	parts := strings.Split(src, "/")
	if len(parts) < 3 {
		return Info{}, fmt.Errorf("import path too short: %q", src)
	}
	host := parts[0]
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
	default:
		return Info{}, fmt.Errorf("unsupported host %q (only github.com, gitlab.com, bitbucket.org)", host)
	}
	repoImport := strings.Join(parts[:3], "/")
	remaining := parts[3:]
	if len(remaining) > 0 && isMajorVersionMarker(remaining[0]) {
		remaining = remaining[1:]
	}
	sub := "."
	if len(remaining) > 0 {
		sub = "./" + strings.Join(remaining, "/")
	}
	return Info{
		RepoURL: "https://" + repoImport + ".git",
		RepoDir: filepath.Join(Home(), "src", repoImport),
		Subpath: sub,
	}, nil
}

func isMajorVersionMarker(seg string) bool {
	if len(seg) < 2 || seg[0] != 'v' {
		return false
	}
	for _, r := range seg[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Runner abstracts how a caller wants exec.Cmd output captured (e.g. piped
// into a per-service log stream). Caller must Start and Wait the command.
type Runner func(ctx context.Context, cmd *exec.Cmd) error

// Ensure makes sure src is checked out at branch under Home()/src. If the
// directory exists already, it fetches+resets to FETCH_HEAD on the requested
// branch (when given). Returns the resolved Info regardless of clone outcome
// so callers can build sub-commands against it.
func Ensure(ctx context.Context, src, branch string, run Runner) (Info, error) {
	info, err := Resolve(src)
	if err != nil {
		return info, err
	}
	if run == nil {
		return info, errors.New("source.Ensure: nil runner")
	}

	if _, err := os.Stat(info.RepoDir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(info.RepoDir), 0o755); err != nil {
			return info, fmt.Errorf("mkdir repo parent: %w", err)
		}
		args := []string{"clone", "--depth", "1", "--recurse-submodules"}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		args = append(args, info.RepoURL, info.RepoDir)
		if err := run(ctx, exec.CommandContext(ctx, "git", args...)); err != nil {
			return info, fmt.Errorf("git clone: %w", err)
		}
		return info, nil
	} else if err != nil {
		return info, err
	}

	fetchArgs := []string{"-C", info.RepoDir, "fetch", "--depth", "1", "origin"}
	if branch != "" {
		fetchArgs = append(fetchArgs, branch)
	}
	if err := run(ctx, exec.CommandContext(ctx, "git", fetchArgs...)); err != nil {
		return info, fmt.Errorf("git fetch: %w", err)
	}
	if err := run(ctx, exec.CommandContext(ctx, "git", "-C", info.RepoDir, "reset", "--hard", "FETCH_HEAD")); err != nil {
		return info, fmt.Errorf("git reset: %w", err)
	}
	return info, nil
}
