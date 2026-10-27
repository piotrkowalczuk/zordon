package control

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestLockAlpha_probeLifecycle(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "alpha.sock")

	if held, pid := ProbeAlpha(sock); held || pid != 0 {
		t.Fatalf("ProbeAlpha before lock = (%v, %d), want (false, 0)", held, pid)
	}

	unlock, err := LockAlpha(t.Context(), sock, nil)
	if err != nil {
		t.Fatalf("LockAlpha: %v", err)
	}

	if held, pid := ProbeAlpha(sock); !held || pid != os.Getpid() {
		unlock()
		t.Fatalf("ProbeAlpha while held = (%v, %d), want (true, %d)", held, pid, os.Getpid())
	}

	unlock()

	if held, _ := ProbeAlpha(sock); held {
		t.Fatal("ProbeAlpha after unlock = true, want false")
	}
}

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

	// Accept one connection and hold it open without replying, simulating a
	// wedged alpha handler.
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
		t.Fatalf("IsNotRunning(%v) = true on a recv timeout; want false (server IS listening, just stuck)", err)
	}

	// Dialing a missing socket must still be classified as IsNotRunning.
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

		// Hold LockAlpha for 6.5s of virtual bubble time so WaitAlphaGone must
		// emit the 1s initial report and the +5s periodic report before
		// returning once unlock() runs.
		go func() {
			time.Sleep(6500 * time.Millisecond)
			unlock()
		}()

		var waitedDurations []time.Duration
		var waitPID int
		start := time.Now()
		err = WaitAlphaGone(t.Context(), sock, 0, 15*time.Second, func(holderPID int, waited time.Duration) {
			waitedDurations = append(waitedDurations, waited.Round(100*time.Millisecond))
			waitPID = holderPID
		}, nil)
		if err != nil {
			t.Fatalf("WaitAlphaGone: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 6500*time.Millisecond {
			t.Fatalf("WaitAlphaGone returned at %s, want >= 6.5s in virtual clock", elapsed)
		}
		if len(waitedDurations) != 2 {
			t.Fatalf("onWait calls = %v, want 2 reports (~1s and ~6s)", waitedDurations)
		}
		if waitedDurations[0] < 1*time.Second || waitedDurations[1] < 6*time.Second {
			t.Errorf("onWait durations = %v, want >=1s then >=6s", waitedDurations)
		}
		if waitPID != os.Getpid() {
			t.Errorf("onWait pid = %d, want %d", waitPID, os.Getpid())
		}
	})
}

func TestWaitAlphaGone_escalatesToSIGKILL(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "alpha.sock")

	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	var forceKilledPID int
	err := WaitAlphaGone(t.Context(), sock, pid, 200*time.Millisecond, nil, func(killedPID int) {
		forceKilledPID = killedPID
	})
	if err != nil {
		t.Fatalf("WaitAlphaGone: %v", err)
	}
	if forceKilledPID != pid {
		t.Errorf("onForceKill pid = %d, want %d", forceKilledPID, pid)
	}
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("child process did not exit after SIGKILL")
	}
}
