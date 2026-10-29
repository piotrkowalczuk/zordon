package zlock

import (
	"errors"
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
