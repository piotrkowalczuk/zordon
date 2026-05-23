package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

// Encodes the contract that fixed the "alpha doesn't die on Ctrl-C"
// stuck-in-build bug: the runner must surface ctx cancellation by
// killing the subprocess group, not by waiting for the process to
// finish on its own. Without this, bringupAndSupervise would block in
// c.Wait(), sc.done would never close, state.shutdownAll would hang
// alpha indefinitely, and pgrep would show one zombie alpha per
// Ctrl-C cycle.

// discardLog gives the runner a real *zlog.Logger that throws output
// away — keeps the test silent without forcing a no-op logger type.
func discardLog(t *testing.T) *zlog.Logger {
	t.Helper()
	return zlog.New(os.NewFile(uintptr(0), os.DevNull), false)
}

// Happy path: subprocess exits naturally before ctx cancels. The
// runner returns whatever c.Wait returned and doesn't fire any kill.
func TestPrepareRunner_normalExitReturnsWaitError(t *testing.T) {
	t.Parallel()
	runner := newPrepareRunner("svc1", newSafeEncoder(nil), discardLog(t))
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := runner(context.Background(), cmd); err != nil {
		t.Fatalf("clean exit: %v", err)
	}

	cmd = exec.Command("/bin/sh", "-c", "exit 7")
	err := runner(context.Background(), cmd)
	if err == nil {
		t.Fatal("non-zero exit must surface as error")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if ee.ExitCode() != 7 {
		t.Fatalf("exit code: got %d, want 7", ee.ExitCode())
	}
}

// THE fix: ctx cancel during a long-running subprocess must reap it
// (SIGTERM → grace → SIGKILL on pgid) and return ctx.Err(), within a
// bounded window. Mirrors zordon Ctrl-C landing while alpha is mid
// `go build` or `cargo install`.
func TestPrepareRunner_ctxCancelReapsSubprocess(t *testing.T) {
	t.Parallel()
	runner := newPrepareRunner("svc1", newSafeEncoder(nil), discardLog(t))
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := runner(ctx, cmd)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// SIGTERM lands quickly; sh exits ~immediately. The 2s grace is
	// the upper bound before SIGKILL fires; total must be well under
	// that for a well-behaved child.
	if elapsed > 3*time.Second {
		t.Fatalf("reaping took %s, expected <3s", elapsed)
	}
	if cmd.Process != nil && processAlive(cmd.Process.Pid) {
		t.Fatalf("pid %d still alive after runner returned", cmd.Process.Pid)
	}
}

// SIGKILL escalation: subprocess ignores SIGTERM (trap), runner must
// fall through to SIGKILL on pgid after the grace window. The
// trap-then-sleep-forever construct is exactly how a misbehaving
// build helper could pin alpha pre-fix.
func TestPrepareRunner_ctxCancelEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	runner := newPrepareRunner("svc1", newSafeEncoder(nil), discardLog(t))
	// Bash trap on TERM ⇒ SIGTERM is no-op; only SIGKILL stops this.
	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := runner(ctx, cmd)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Grace is 2s in newPrepareRunner; total bound: cancel-delay
	// (50ms) + grace (2s) + slack < 4s.
	if elapsed > 4*time.Second {
		t.Fatalf("SIGKILL escalation took %s, expected <4s", elapsed)
	}
	if elapsed < 1500*time.Millisecond {
		// Suspiciously fast — SIGTERM should NOT have killed a TRAPed
		// shell, so we must have waited the grace. If we didn't, the
		// trap broke silently and this test doesn't actually cover
		// the escalation path.
		t.Fatalf("returned in %s — too fast; SIGTERM trap may not be in effect, escalation path not exercised", elapsed)
	}
	if cmd.Process != nil && processAlive(cmd.Process.Pid) {
		t.Fatalf("pid %d still alive after SIGKILL window", cmd.Process.Pid)
	}
}

// Setpgid must be forced even when the caller provided SysProcAttr,
// otherwise the kill-pgid logic in the cancel path would target
// alpha's own group (catastrophic) or just the parent shell (leaking
// grandchildren). prepareBuild already sets Setpgid itself, but the
// runner can't trust callers — git subprocesses in source/* don't.
func TestPrepareRunner_forcesSetpgidPreservingOtherFlags(t *testing.T) {
	t.Parallel()
	runner := newPrepareRunner("svc1", newSafeEncoder(nil), discardLog(t))
	cmd := exec.Command("/bin/sh", "-c", "true")
	// Pre-existing SysProcAttr with some unrelated flag set, to prove
	// the runner doesn't blow it away.
	cmd.SysProcAttr = &syscall.SysProcAttr{Foreground: false}

	if err := runner(context.Background(), cmd); err != nil {
		t.Fatalf("runner: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("Setpgid not forced on; SysProcAttr=%#v", cmd.SysProcAttr)
	}
}

// processAlive — same helper as cmd/zordon. Duplicated here because
// the two packages don't share a utility module and we want unit
// tests to live alongside the code they exercise. (Drop if this ever
// gets factored into a shared internal/process helper.)
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
