package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zlock"
)

func TestWaitAlphaGone_deadPIDNotReportedAsTearingDown(t *testing.T) {
	cases := map[string]struct {
		passOldPID bool
	}{
		"no_live_old_state": {passOldPID: false},
		"old_pid_died":      {passOldPID: true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			wsDir := t.TempDir()
			kc := zlock.NewGlobalKeychain(t.TempDir()).ForWorkspace(wsDir)
			deadPID := reapedChildPID(t)
			holdRawLockWithPID(t, filepath.Join(wsDir, "alpha.lock"), deadPID)

			oldPID := 0
			if c.passOldPID {
				oldPID = deadPID
			}
			sock := filepath.Join(t.TempDir(), "alpha.sock")

			err := waitAlphaGone(t.Context(), kc, sock, oldPID, true, 120*time.Millisecond, nil)
			if err == nil {
				t.Fatal("waitAlphaGone = nil while alpha.lock is held; want timeout error")
			}
			msg := err.Error()
			notWantTeardown := fmt.Sprintf("old alpha (pid=%d) still tearing down", deadPID)
			if strings.Contains(msg, notWantTeardown) {
				t.Errorf("error %q falsely reports dead alpha as still tearing down", msg)
			}
			notWantHeld := fmt.Sprintf("held by pid=%d", deadPID)
			if strings.Contains(msg, notWantHeld) {
				t.Errorf("error %q falsely reports dead alpha as lock holder", msg)
			}
			wantStale := fmt.Sprintf("stale pid=%d", deadPID)
			if !strings.Contains(msg, wantStale) || !strings.Contains(msg, "held by another process") {
				t.Errorf("error %q missing stale-PID diagnostic (%q / \"held by another process\")", msg, wantStale)
			}
		})
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
