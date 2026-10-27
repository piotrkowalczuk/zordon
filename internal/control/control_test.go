package control

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/daemon"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestRoundtrip_stuckServerTimesOutAndIsNotClassifiedAsNotRunning(t *testing.T) {
	// Keep path short so AF_UNIX sun_path (104 bytes on darwin) never overflows.
	dir, err := os.MkdirTemp("/tmp", "zctrl-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")

	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-done
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = Roundtrip(ctx, sock, &protocol.Request{Op: protocol.OpState})
	if err == nil {
		t.Fatal("Roundtrip against stuck server succeeded; want timeout error")
	}
	if IsNotRunning(err) {
		t.Fatalf("IsNotRunning(%v) = true on a recv timeout; want false", err)
	}

	_, missingErr := Roundtrip(t.Context(), filepath.Join(dir, "missing.sock"), &protocol.Request{Op: protocol.OpState})
	if !IsNotRunning(missingErr) {
		t.Fatalf("IsNotRunning(%v) = false on missing socket; want true", missingErr)
	}
}

func TestWaitAlphaGone_waitsForAlphaLockReleaseAndReportsWait(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "alpha.sock")

	synctest.Test(t, func(t *testing.T) {
		unlock, err := LockAlpha(t.Context(), sock, nil)
		if err != nil {
			t.Fatalf("LockAlpha: %v", err)
		}

		go func() {
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-t.Context().Done():
			}
			unlock()
		}()

		var reports int
		var waitPID int
		err = WaitAlphaGone(t.Context(), sock, 0, 15*time.Second, func(holderPID int, _ time.Duration) {
			reports++
			waitPID = holderPID
		}, nil)
		if err != nil {
			t.Fatalf("WaitAlphaGone: %v", err)
		}
		if reports != 1 || waitPID != os.Getpid() {
			t.Fatalf("reports=%d pid=%d, want 1 and %d", reports, waitPID, os.Getpid())
		}
	})
}

func TestWaitAlphaGone_escalatesToSIGKILL(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "alpha.sock")

	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	proc := daemon.Wait(cmd)
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var forceKilledPID int
	err := WaitAlphaGone(t.Context(), sock, cmd.Process.Pid, 200*time.Millisecond, nil, func(killedPID int) {
		forceKilledPID = killedPID
	})
	if err != nil {
		t.Fatalf("WaitAlphaGone: %v", err)
	}
	if forceKilledPID != cmd.Process.Pid {
		t.Errorf("onForceKill pid = %d, want %d", forceKilledPID, cmd.Process.Pid)
	}
	<-proc.Done()
}
