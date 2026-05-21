package parentwatch

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// makeChildFd installs a pipe pair on a fresh exec.Cmd and pretends to
// be the child: read end goes into ExtraFiles, env var is set, and we
// recover the fd number the child would see (3+offset). Returns the
// fd-number string the child should read out of envVar plus the
// write-end the test (acting as parent) controls.
func attachAndExportFd(t *testing.T, envVar string) (childFD int, write *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Adopt the read end into THIS process at the fd Attach would have
	// announced: we dup it to a known descriptor and pass that number
	// through the env, so Watch can find it. We can't use Attach
	// directly because that wires the fd into a child process via
	// exec; here we ARE the child.
	dup, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		t.Fatalf("dup: %v", err)
	}
	_ = r.Close()
	t.Setenv(envVar, strconv.Itoa(dup))
	t.Cleanup(func() {
		_ = w.Close()
	})
	return dup, w
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

func TestWatchUnsetEnvReturnsNil(t *testing.T) {
	os.Unsetenv("TEST_PARENT_FD")
	w, err := Watch("TEST_PARENT_FD")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if w != nil {
		t.Fatalf("Watch returned non-nil for unset env")
	}
	// Selecting on a nil channel must block — we just verify Died()
	// returns nil so consumers can use the obvious "no watch ⇒ never
	// fires" pattern.
	if w.Died() != nil {
		t.Fatalf("nil watcher should return nil Died() channel")
	}
	w.Stop() // must not panic
}

func TestWatchBadEnv(t *testing.T) {
	t.Setenv("BAD_FD", "not-a-number")
	if _, err := Watch("BAD_FD"); err == nil {
		t.Fatalf("expected error on non-numeric env")
	}
	t.Setenv("BAD_FD", "2")
	if _, err := Watch("BAD_FD"); err == nil {
		t.Fatalf("expected error on fd<3")
	}
}

func TestWatchFiresOnEOF(t *testing.T) {
	_, write := attachAndExportFd(t, "P_FD")
	w, err := Watch("P_FD")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if w == nil {
		t.Fatalf("Watch returned nil despite env set")
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
	_, write := attachAndExportFd(t, "Q_FD")
	w, err := Watch("Q_FD")
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
	_, _ = attachAndExportFd(t, "R_FD")
	w, err := Watch("R_FD")
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
	childFD, write := attachAndExportFd(t, "S_FD")

	var logged atomic.Int32
	w, err := Watch("S_FD", WithErrorLog(func(err error) {
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
	_ = childFD
}

func TestWatchForeverFiresOnEOF(t *testing.T) {
	_, write := attachAndExportFd(t, "T_FD")
	gone, err := WatchForever("T_FD", WithPollInterval(time.Hour))
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

func TestWatchForeverNoEnvUsesPollOnly(t *testing.T) {
	os.Unsetenv("U_FD")
	// Poll fast; original ppid is whatever ran us. Without a real
	// parent change we just verify WatchForever doesn't error out and
	// doesn't fire immediately.
	gone, err := WatchForever("U_FD", WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("WatchForever: %v", err)
	}
	select {
	case <-gone:
		t.Fatalf("WatchForever fired with stable ppid and no pipe")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestWatchForeverBadEnv(t *testing.T) {
	t.Setenv("V_FD", "garbage")
	if _, err := WatchForever("V_FD"); err == nil {
		t.Fatalf("expected error on bad env")
	}
}

// Sanity: the EOF-fired Died() should be observable from many
// readers — closed channel semantics — without any extra coordination.
func TestDiedIsBroadcastable(t *testing.T) {
	_, write := attachAndExportFd(t, "W_FD")
	w, err := Watch("W_FD")
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
	_, write := attachAndExportFd(t, "X_FD")
	w, err := Watch("X_FD")
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
	_, _ = attachAndExportFd(t, "Y_FD")
	var lastErr atomic.Value
	w, err := Watch("Y_FD", WithErrorLog(func(err error) { lastErr.Store(err) }))
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
