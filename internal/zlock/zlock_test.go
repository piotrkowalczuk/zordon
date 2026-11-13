package zlock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
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
		if kc.IsAlphaLockedBy(os.Getpid()) {
			t.Fatal("IsAlphaLockedBy true before LockAlpha")
		}
		unlock, err := kc.LockAlpha(t.Context())
		if err != nil {
			t.Fatalf("LockAlpha: %v", err)
		}
		if !kc.IsAlphaLockedBy(os.Getpid()) {
			t.Fatal("IsAlphaLockedBy(self) false while holding LockAlpha")
		}
		if kc.IsAlphaLockedBy(os.Getpid() + 100000) {
			t.Fatal("IsAlphaLockedBy(otherPID) true while held by self")
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
		if kc.IsAlphaLockedBy(os.Getpid()) {
			t.Fatal("IsAlphaLockedBy true after unlock")
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

func TestWorkspaceKeychain_IsAlphaLockedBy_deadPID(t *testing.T) {
	wsDir := t.TempDir()
	kc := NewGlobalKeychain(t.TempDir()).ForWorkspace(wsDir)
	deadPID := reapedChildPID(t)

	holdRawLockWithPID(t, filepath.Join(wsDir, "alpha.lock"), deadPID)

	if kc.IsAlphaLockedBy(deadPID) {
		t.Errorf("IsAlphaLockedBy(%d) = true for dead process; want false", deadPID)
	}
	if !kc.IsAlphaLockedBy(0) {
		t.Error("IsAlphaLockedBy(0) = false while alpha.lock is held by another fd; want true")
	}
}

func reapedChildPID(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run no-op child: %v", err)
	}
	return cmd.Process.Pid
}

func holdRawLockWithPID(t *testing.T, lockPath string, pid int) {
	t.Helper()
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("flock %s: %v", lockPath, err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		t.Fatalf("write pid to %s: %v", lockPath, err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}
