package zlock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestWorkspaceKeychain_TryLockZordon(t *testing.T) {
	kc := NewGlobalKeychain(t.TempDir()).ForWorkspace(t.TempDir())

	unlock, err := kc.TryLockZordon()
	if err != nil {
		t.Fatalf("TryLockZordon: %v", err)
	}
	if _, err := kc.TryLockZordon(); !errors.Is(err, ErrLocked) {
		unlock()
		t.Fatalf("second TryLockZordon = %v, want ErrLocked", err)
	}
	unlock()

	unlock2, err := kc.TryLockZordon()
	if err != nil {
		t.Fatalf("TryLockZordon after unlock: %v", err)
	}
	unlock2()
}

func TestWorkspaceKeychain_LockAlphaAndWaitUnlockedAlpha(t *testing.T) {
	kc := NewGlobalKeychain(t.TempDir()).ForWorkspace(t.TempDir())

	synctest.Test(t, func(t *testing.T) {
		unlock, err := kc.LockAlpha(t.Context())
		if err != nil {
			t.Fatalf("LockAlpha: %v", err)
		}
		go func() {
			select {
			case <-time.After(150 * time.Millisecond):
			case <-t.Context().Done():
			}
			unlock()
		}()

		if err := kc.WaitUnlockedAlpha(t.Context()); err != nil {
			t.Fatalf("WaitUnlockedAlpha: %v", err)
		}
	})
}

func TestGlobalKeychain(t *testing.T) {
	kc := NewGlobalKeychain(t.TempDir())
	ctx := t.Context()

	unlockReg, err := kc.LockRegistry(ctx)
	if err != nil {
		t.Fatalf("LockRegistry: %v", err)
	}
	unlockReg()

	unlockMise, err := kc.LockMise(ctx)
	if err != nil {
		t.Fatalf("LockMise: %v", err)
	}
	unlockMise()

	unlockTool, err := kc.LockTool(ctx, "aqua:etcd-io/etcd", "3.5")
	if err != nil {
		t.Fatalf("LockTool: %v", err)
	}
	unlockTool()

	unlockRepo, err := kc.LockRepo(ctx, "github.com/acme/infra")
	if err != nil {
		t.Fatalf("LockRepo: %v", err)
	}
	unlockRepo()
}

func TestGlobalKeychain_LockPins(t *testing.T) {
	kc := NewGlobalKeychain(t.TempDir())
	project := filepath.Join(t.TempDir(), "zordon.lock")

	unlock, err := kc.LockPins(t.Context(), project)
	if err != nil {
		t.Fatalf("LockPins: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := kc.LockPins(ctx, project); !errors.Is(err, context.DeadlineExceeded) {
		unlock()
		t.Fatalf("second LockPins of one project = %v, want it to wait", err)
	}
	other, err := kc.LockPins(t.Context(), filepath.Join(t.TempDir(), "zordon.lock"))
	if err != nil {
		unlock()
		t.Fatalf("LockPins of another project: %v", err)
	}
	other()
	unlock()

	again, err := kc.LockPins(t.Context(), project)
	if err != nil {
		t.Fatalf("LockPins after unlock: %v", err)
	}
	again()
}
