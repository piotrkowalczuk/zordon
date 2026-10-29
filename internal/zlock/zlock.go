// Package zlock is the single catalog of process-wide file locks in zordon.
// Locks are split by scope into two distinct types matching the lock hierarchy:
// WorkspaceKeychain (per-workspace lifecycle locks) and GlobalKeychain
// (host-wide resource locks under ZORDON_HOME).
package zlock

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

type UnlockFunc = zfs.UnlockFunc
type LockError = zfs.LockError

var ErrLocked = zfs.ErrLocked

// GlobalKeychain holds host-wide resource locks rooted at ZORDON_HOME (~/.zordon).
type GlobalKeychain struct {
	homeDir string
}

func NewGlobalKeychain(homeDir string) GlobalKeychain {
	if strings.TrimSpace(homeDir) == "" {
		panic("zlock: NewGlobalKeychain requires non-empty homeDir")
	}
	return GlobalKeychain{homeDir: homeDir}
}

func (k GlobalKeychain) ForWorkspace(workspaceDir string) WorkspaceKeychain {
	return NewWorkspaceKeychain(workspaceDir)
}

// LockRegistry takes an exclusive lock on <homeDir>/registry.lock.
func (k GlobalKeychain) LockRegistry(ctx context.Context) (UnlockFunc, error) {
	return zfs.Lock(ctx, k.revealHome(), "registry")
}

// LockMise takes an exclusive lock on <homeDir>/locks/mise.lock.
func (k GlobalKeychain) LockMise(ctx context.Context) (UnlockFunc, error) {
	return zfs.Lock(ctx, filepath.Join(k.revealHome(), "locks"), "mise")
}

// LockTool takes an exclusive lock on <homeDir>/locks/tool-<tool>-<version>.lock.
func (k GlobalKeychain) LockTool(ctx context.Context, tool, version string) (UnlockFunc, error) {
	if strings.TrimSpace(tool) == "" || strings.TrimSpace(version) == "" {
		panic("zlock: GlobalKeychain.LockTool requires non-empty tool and version")
	}
	return zfs.Lock(ctx, filepath.Join(k.revealHome(), "locks"), "tool-"+tool+"-"+version)
}

// LockRepo takes an exclusive lock on <homeDir>/locks/repo-<repo>.lock.
func (k GlobalKeychain) LockRepo(ctx context.Context, repo string) (UnlockFunc, error) {
	if strings.TrimSpace(repo) == "" {
		panic("zlock: GlobalKeychain.LockRepo requires non-empty repo")
	}
	return zfs.Lock(ctx, filepath.Join(k.revealHome(), "locks"), "repo-"+repo)
}

// WorkspaceKeychain holds per-workspace lifecycle locks rooted at <projectRoot>/workspaces/<workspace>.
type WorkspaceKeychain struct {
	workspaceDir string
}

func NewWorkspaceKeychain(workspaceDir string) WorkspaceKeychain {
	if strings.TrimSpace(workspaceDir) == "" {
		panic("zlock: NewWorkspaceKeychain requires non-empty workspaceDir")
	}
	return WorkspaceKeychain{workspaceDir: workspaceDir}
}

// TryLockZordon takes a non-blocking lock on <workspaceDir>/zordon.lock.
func (k WorkspaceKeychain) TryLockZordon() (UnlockFunc, error) {
	return zfs.TryLock(k.revealWorkspace(), "zordon")
}

// LockAlpha takes an exclusive lock on <workspaceDir>/alpha.lock held for the
// lifetime of an alpha daemon (including its shutdownAll teardown phase).
func (k WorkspaceKeychain) LockAlpha(ctx context.Context) (UnlockFunc, error) {
	return zfs.Lock(ctx, k.revealWorkspace(), "alpha")
}

// WaitUnlockedAlpha blocks until no alpha daemon holds <workspaceDir>/alpha.lock or ctx expires.
func (k WorkspaceKeychain) WaitUnlockedAlpha(ctx context.Context) error {
	return zfs.WaitUnlocked(ctx, k.revealWorkspace(), "alpha")
}

func (k GlobalKeychain) revealHome() string {
	if k.homeDir == "" {
		panic("zlock: zero-value GlobalKeychain used — construct with NewGlobalKeychain")
	}
	return k.homeDir
}

func (k WorkspaceKeychain) revealWorkspace() string {
	if k.workspaceDir == "" {
		panic("zlock: zero-value WorkspaceKeychain used — construct with NewWorkspaceKeychain")
	}
	return k.workspaceDir
}
