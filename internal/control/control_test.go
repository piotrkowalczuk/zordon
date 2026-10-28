package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestAlphaSocketPath_Roundtrip_stuckServerTimesOutAndIsNotClassifiedAsNotRunning(t *testing.T) {
	// Keep path short so AF_UNIX sun_path (104 bytes on darwin) never overflows.
	dir, err := os.MkdirTemp("/tmp", "zctrl-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := AlphaSocketPath(filepath.Join(dir, "a.sock"))

	ln, err := sock.Listen()
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
	_, err = sock.Roundtrip(ctx, &protocol.Request{Op: protocol.OpState})
	if err == nil {
		t.Fatal("Roundtrip against stuck server succeeded; want timeout error")
	}
	if IsNotRunning(err) {
		t.Fatalf("IsNotRunning(%v) = true on a recv timeout; want false", err)
	}

	missingSock := AlphaSocketPath(filepath.Join(dir, "missing.sock"))
	_, missingErr := missingSock.Roundtrip(t.Context(), &protocol.Request{Op: protocol.OpState})
	if !IsNotRunning(missingErr) {
		t.Fatalf("IsNotRunning(%v) = false on missing socket; want true", missingErr)
	}
}

func TestAlphaSocketPath_WaitGone_waitsForAlphaLockReleaseAndReportsWait(t *testing.T) {
	sock := AlphaSocketPath(filepath.Join(t.TempDir(), "alpha.sock"))

	synctest.Test(t, func(t *testing.T) {
		unlock, err := sock.AcquireLock(t.Context(), nil)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
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
		err = sock.WaitGone(t.Context(), 0, 15*time.Second, func(holderPID int, _ time.Duration) {
			reports++
			waitPID = holderPID
		})
		if err != nil {
			t.Fatalf("WaitGone: %v", err)
		}
		if reports != 1 || waitPID != os.Getpid() {
			t.Fatalf("reports=%d pid=%d, want 1 and %d", reports, waitPID, os.Getpid())
		}
	})
}

func TestAlphaSocketPath_WaitGone_timesOutWithoutKilling(t *testing.T) {
	sock := AlphaSocketPath(filepath.Join(t.TempDir(), "alpha.sock"))

	synctest.Test(t, func(t *testing.T) {
		unlock, err := sock.AcquireLock(t.Context(), nil)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		defer unlock()

		err = sock.WaitGone(t.Context(), 0, 2*time.Second, nil)
		if err == nil || !strings.Contains(err.Error(), "still running after") {
			t.Fatalf("WaitGone err = %v, want timeout error", err)
		}
	})
}
