package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// spawnSleeper starts `sleep <secs>` in its own process group so callers
// can SIGKILL via -pgid the same way alpha's restart path does. A
// background goroutine reaps the child the moment it exits, so kill(pid,
// 0) reports ESRCH promptly — mirrors production, where the old alpha is
// orphaned (its spawning zordon already exited) and init reaps it. Without
// this the child lingers as a zombie and processAlive stays true.
func spawnSleeper(t *testing.T, secs string) (pid int, cleanup func()) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep "+secs)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid = cmd.Process.Pid
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	cleanup = func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-waited
	}
	return pid, cleanup
}

// Encodes the bug fix's contract: callers may not yet know a PID
// (legacy paths, missing OpState). The function must not block or
// signal in that case — just return.
func TestWaitProcessGone_zeroPidReturnsImmediately(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if err := waitProcessGone(context.Background(), 0, 5*time.Second); err != nil {
		t.Fatalf("waitProcessGone(0): %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("waitProcessGone(0) took %s, expected immediate", d)
	}
}

// A PID that never existed (or one already reaped) must look gone
// immediately — kill(pid, 0) returns ESRCH, processAlive false, no
// wait, no escalation.
func TestWaitProcessGone_alreadyDeadReturnsNil(t *testing.T) {
	t.Parallel()
	// pid=1 always exists (init); we instead test with an obviously
	// unused high pid. On macOS/Linux the PID space is bounded but
	// `kill(0x7fffffff, 0)` reliably returns ESRCH on test hosts.
	if err := waitProcessGone(context.Background(), 0x7fffffff, 5*time.Second); err != nil {
		t.Fatalf("waitProcessGone(huge): %v", err)
	}
}

// The straight-line happy path: alpha process is shutting down,
// finishes within the grace window, waitProcessGone returns nil with
// no SIGKILL fired.
func TestWaitProcessGone_returnsAfterNaturalExit(t *testing.T) {
	t.Parallel()
	pid, cleanup := spawnSleeper(t, "0.2")
	defer cleanup()

	start := time.Now()
	if err := waitProcessGone(context.Background(), pid, 5*time.Second); err != nil {
		t.Fatalf("waitProcessGone: %v", err)
	}
	d := time.Since(start)
	if d < 100*time.Millisecond {
		t.Fatalf("returned too fast (%s) — sleeper should still have been alive at start", d)
	}
	if d > 3*time.Second {
		t.Fatalf("returned too slow (%s) — process exited but poll didn't notice", d)
	}
}

// The actual fix's teeth: a wedged alpha (here: sleeper that outlives
// the timeout) must be SIGKILL'd via the process group and reported as
// gone. Mirrors the production scenario where shutdownAll is stuck on
// a service that ignores SIGTERM.
func TestWaitProcessGone_escalatesToSIGKILLOnTimeout(t *testing.T) {
	t.Parallel()
	pid, cleanup := spawnSleeper(t, "30")
	defer cleanup()

	// Tiny timeout so the deadline trips fast; the SIGKILL pass is
	// what should actually end the test.
	start := time.Now()
	if err := waitProcessGone(context.Background(), pid, 150*time.Millisecond); err != nil {
		t.Fatalf("waitProcessGone: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("escalation took %s — SIGKILL should reap within ms", d)
	}
	if processAlive(pid) {
		t.Fatalf("pid %d still alive after waitProcessGone returned nil", pid)
	}
}

// Context cancel during the wait loop must propagate as ctx.Err() —
// keeps the function composable with the per-level WithTimeout that
// federation.go already uses around spawnAlpha.
func TestWaitProcessGone_respectsContextCancel(t *testing.T) {
	t.Parallel()
	pid, cleanup := spawnSleeper(t, "30")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := waitProcessGone(ctx, pid, 10*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// We bailed out, not killed — the sleeper should still be alive.
	// (cleanup will reap it.) This guards against future "always
	// SIGKILL on exit" changes that would surprise callers who used
	// cancellation as a non-destructive bail.
	if !processAlive(pid) {
		t.Fatalf("waitProcessGone killed the process on ctx cancel; expected non-destructive bail")
	}
}

// processAlive's EPERM-as-alive branch is hard to exercise without
// root, but the ESRCH and "alive" branches are easy and worth pinning:
// they are the whole basis for the spawn-or-not decision in
// federation.go.
func TestProcessAlive(t *testing.T) {
	t.Parallel()
	if processAlive(0) {
		t.Fatal("processAlive(0) must be false")
	}
	if processAlive(-1) {
		t.Fatal("processAlive(-1) must be false")
	}
	if processAlive(0x7fffffff) {
		t.Fatal("processAlive(huge-unused-pid) expected false (ESRCH)")
	}
	pid, cleanup := spawnSleeper(t, "5")
	defer cleanup()
	if !processAlive(pid) {
		t.Fatalf("processAlive(%d) expected true for live sleeper", pid)
	}
}
