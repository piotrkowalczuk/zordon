package main

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
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
// Must use io.Discard, NOT os.NewFile(uintptr(0), …): that wraps fd 0
// (stdin), and closing/finalizing it frees fd 0 for reuse — which under
// `go test -coverprofile` the coverage writer then grabs, only to have a
// stale wrapper close it, surfacing as "write _cover_.out: bad file
// descriptor". io.Discard holds no descriptor at all.
func discardLog(t *testing.T) *zlog.Logger {
	t.Helper()
	return zlog.New(io.Discard, false)
}

// Happy path: subprocess exits naturally before ctx cancels. The
// runner returns whatever c.Wait returned and doesn't fire any kill.
func TestPrepareRunner_normalExitReturnsWaitError(t *testing.T) {
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
	runner := newPrepareRunner("svc1", newSafeEncoder(nil), discardLog(t))
	// Shell traps TERM (⇒ SIGTERM is a no-op; only SIGKILL stops it), then
	// touches the marker passed as $0. Gating the cancel on that marker
	// removes the race the old fixed 50ms timer had: under load the trap
	// might not be installed yet, SIGTERM would kill an un-trapped shell
	// outright, and the escalation path would never be exercised.
	ready := filepath.Join(t.TempDir(), "trapped")
	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; : > "$0"; sleep 30`, ready)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner(ctx, cmd) }()

	waitForFile(t, ready, 5*time.Second) // trap is installed once it exists

	start := time.Now()
	cancel()
	err := <-done
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// SIGTERM is trapped, so the runner must wait the full 2s grace before
	// SIGKILL: bound the window at grace (2s) + slack < 4s, measured from
	// cancel. It must NOT return early — that would mean SIGTERM killed an
	// un-trapped shell and the escalation path was skipped.
	if elapsed > 4*time.Second {
		t.Fatalf("SIGKILL escalation took %s, expected <4s", elapsed)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("returned in %s — too fast; SIGTERM trap not exercised, escalation path skipped", elapsed)
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

// waitForFile blocks until path exists or timeout elapses, polling so the
// test can synchronize on a marker a subprocess writes.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if zfs.Exists(path) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %s", path, timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
