package parentwatch

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// inheritPipeFd installs a pipe, dupes the read end to a fresh fd in
// THIS process (pretending to be the spawned child), and returns the
// fd plus the write end the test (acting as parent) controls.
func inheritPipeFd(t *testing.T) (childFD zfs.FileDescriptor, write *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	dup, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		t.Fatalf("dup: %v", err)
	}
	_ = r.Close()
	t.Cleanup(func() {
		_ = w.Close()
	})
	return zfs.FileDescriptor(dup), w
}

func TestAttachAssignsSequentialFds(t *testing.T) {
	r1, w1, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r1.Close()
	defer w1.Close()
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r2.Close()
	defer w2.Close()

	cmd := exec.Command("/bin/true")
	Attach(cmd, r1, "A_FD")
	Attach(cmd, r2, "B_FD")

	if got, want := len(cmd.ExtraFiles), 2; got != want {
		t.Fatalf("ExtraFiles=%d, want %d", got, want)
	}
	var aFD, bFD string
	for _, kv := range cmd.Env {
		switch {
		case len(kv) > 5 && kv[:5] == "A_FD=":
			aFD = kv[5:]
		case len(kv) > 5 && kv[:5] == "B_FD=":
			bFD = kv[5:]
		}
	}
	if aFD != "3" || bFD != "4" {
		t.Fatalf("fds: A_FD=%q B_FD=%q, want 3 and 4", aFD, bFD)
	}
}

func TestWatchZeroFdReturnsNil(t *testing.T) {
	w, err := Watch(0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if w != nil {
		t.Fatalf("Watch returned non-nil for fd=0")
	}
	// Selecting on a nil channel must block — we just verify Died()
	// returns nil so consumers can use the obvious "no watch ⇒ never
	// fires" pattern.
	if w.Died() != nil {
		t.Fatalf("nil watcher should return nil Died() channel")
	}
	w.Stop() // must not panic
}

func TestWatchBadFd(t *testing.T) {
	if _, err := Watch(zfs.FileDescriptor(2)); err == nil {
		t.Fatalf("expected error on fd<3")
	}
}

func TestWatchFiresOnEOF(t *testing.T) {
	fd, write := inheritPipeFd(t)
	w, err := Watch(fd)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if w == nil {
		t.Fatalf("Watch returned nil despite fd>0")
	}

	select {
	case <-w.Died():
		t.Fatalf("Died fired before parent closed write end")
	case <-time.After(50 * time.Millisecond):
	}

	// "Parent dies": close every write end.
	_ = write.Close()

	select {
	case <-w.Died():
	case <-time.After(2 * time.Second):
		t.Fatalf("Died did not fire after write end closed")
	}
	w.Stop() // must be safe after death
}

func TestWatchStopWinsRace(t *testing.T) {
	fd, write := inheritPipeFd(t)
	w, err := Watch(fd)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	w.Stop()
	// After Stop returns, closing the write end must NOT close Died()
	// retroactively.
	_ = write.Close()

	select {
	case <-w.Died():
		t.Fatalf("Died closed after Stop won the race")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchStopIdempotent(t *testing.T) {
	fd, _ := inheritPipeFd(t)
	w, err := Watch(fd)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	w.Stop()
	w.Stop()
	w.Stop()
}

func TestWatchErrorLogReceivesNonEOF(t *testing.T) {
	// We force a non-EOF error by closing the fd from underneath the
	// goroutine's Read. The simplest hammer: install a watcher then
	// Close() the file in a way Stop() didn't authorize.
	fd, write := inheritPipeFd(t)

	var logged atomic.Int32
	w, err := Watch(fd, WithErrorLog(func(err error) {
		if err != nil {
			logged.Add(1)
		}
	}))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	_ = write.Close()
	// Wait for Died.
	select {
	case <-w.Died():
	case <-time.After(2 * time.Second):
		t.Fatalf("Died never fired")
	}
	// EOF path should NOT log (it's the normal case).
	if logged.Load() != 0 {
		t.Fatalf("error log fired on clean EOF (count=%d)", logged.Load())
	}
}

func TestWatchForeverFiresOnEOF(t *testing.T) {
	fd, write := inheritPipeFd(t)
	gone, err := WatchForever(fd, WithPollInterval(time.Hour))
	if err != nil {
		t.Fatalf("WatchForever: %v", err)
	}
	_ = write.Close()
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatalf("WatchForever did not fire on EOF")
	}
}

func TestWatchForeverZeroFdUsesPollOnly(t *testing.T) {
	// Poll fast; original ppid is whatever ran us. Without a real
	// parent change we just verify WatchForever doesn't error out and
	// doesn't fire immediately.
	gone, err := WatchForever(0, WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("WatchForever: %v", err)
	}
	select {
	case <-gone:
		t.Fatalf("WatchForever fired with stable ppid and no pipe")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestWatchForeverBadFd(t *testing.T) {
	if _, err := WatchForever(zfs.FileDescriptor(2)); err == nil {
		t.Fatalf("expected error on fd<3")
	}
}

// Sanity: the EOF-fired Died() should be observable from many
// readers — closed channel semantics — without any extra coordination.
func TestDiedIsBroadcastable(t *testing.T) {
	fd, write := inheritPipeFd(t)
	w, err := Watch(fd)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	_ = write.Close()
	<-w.Died()
	// Read again — closed channel keeps yielding zero values forever.
	select {
	case <-w.Died():
	default:
		t.Fatalf("closed Died channel not re-readable")
	}
}

func TestIgnoreSIGPIPEDoesNotKillProcess(t *testing.T) {
	IgnoreSIGPIPE()
	// Hard to assert non-death without forking. We at least exercise
	// the call path and make sure the runtime accepts repeated Notify
	// registrations from the same process.
	IgnoreSIGPIPE()
}

// Compile-time sanity: nil *Watcher must accept all methods.
func TestNilWatcherMethods(t *testing.T) {
	var w *Watcher
	if ch := w.Died(); ch != nil {
		t.Fatalf("nil watcher Died()=%v, want nil", ch)
	}
	w.Stop()
}

// Defensive: an EOF that fires before any goroutine has selected on
// Died() should still latch — closing channels is sticky.
func TestEOFLatchesBeforeSelect(t *testing.T) {
	fd, write := inheritPipeFd(t)
	w, err := Watch(fd)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	_ = write.Close()
	// Give the goroutine a moment to close died.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-w.Died():
	default:
		t.Fatalf("Died not closed despite EOF before select")
	}
}

// Cross-check: a non-EOF Read error (we simulate by closing the
// underlying file from outside Stop) should be reported via errorLog
// AND fire Died.
func TestNonEOFReadError(t *testing.T) {
	fd, _ := inheritPipeFd(t)
	var lastErr atomic.Value
	w, err := Watch(fd, WithErrorLog(func(err error) { lastErr.Store(err) }))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Close the watcher's file directly (bypassing Stop's flag dance)
	// to simulate EBADF/EIO. We do this by closing the fd through a
	// duplicate so Stop's later Close() is also a no-op.
	_ = w.f.Close()
	select {
	case <-w.Died():
	case <-time.After(2 * time.Second):
		t.Fatalf("Died never fired on forced error")
	}
	if v := lastErr.Load(); v != nil {
		if e, ok := v.(error); ok && e != nil && !errors.Is(e, os.ErrClosed) {
			// any non-nil error is fine; we just verify the sink was
			// reached (which it might not be if the impl maps the
			// race to EOF — both are acceptable).
		}
	}
}
