package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// fakeUntil cancels the daemon's context after a delay — a controllable external
// stop source for exercising Run without a real pipe or signal.
type fakeUntil struct {
	reason   string
	delay    time.Duration
	disarmed chan struct{}
}

func (f *fakeUntil) arm(cancel context.CancelCauseFunc) (func(), error) {
	go func() {
		time.Sleep(f.delay)
		cancel(errors.New(f.reason))
	}()
	return func() { close(f.disarmed) }, nil
}

func TestDaemon_Run_bodyReturns(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		want := errors.New("boom")
		err := New().Run(context.Background(), func(context.Context) error { return want }, Forever())
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v (passed straight through)", err, want)
		}
	})
	t.Run("nil", func(t *testing.T) {
		err := New().Run(context.Background(), func(context.Context) error { return nil }, Forever())
		if err != nil {
			t.Fatalf("err = %v, want nil (clean completion passed through)", err)
		}
	})
}

func TestDaemon_Run_contextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	var cause error
	err := New().Run(ctx, func(ctx context.Context) error {
		<-ctx.Done()
		cause = context.Cause(ctx)
		return nil
	}, Forever())
	if err != nil {
		t.Fatalf("err = %v, want nil (body wound down cleanly)", err)
	}
	if !errors.Is(cause, context.Canceled) {
		t.Fatalf("context.Cause = %v, want context.Canceled", cause)
	}
}

func TestDaemon_Run_untilFired(t *testing.T) {
	u := &fakeUntil{reason: "parent gone (test)", delay: 50 * time.Millisecond, disarmed: make(chan struct{})}
	var cause error
	err := New().Run(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		cause = context.Cause(ctx)
		return nil
	}, u)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cause == nil || cause.Error() != "parent gone (test)" {
		t.Fatalf("context.Cause = %v, want the until's reason", cause)
	}
	select {
	case <-u.disarmed:
	default:
		t.Fatal("until was not disarmed")
	}
}

func TestDaemon_Run_signal(t *testing.T) {
	var cause error
	err := New().Run(context.Background(), func(ctx context.Context) error {
		// Run armed signal.Notify before calling us, so the signal lands on its
		// channel and cancels ctx rather than killing the test process.
		go func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }()
		<-ctx.Done()
		cause = context.Cause(ctx)
		return nil
	}, Forever())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cause == nil || !strings.Contains(cause.Error(), "signal:") {
		t.Fatalf("context.Cause = %v, want a signal: cause", cause)
	}
}

func TestLifeline_firesOnPipeEOF(t *testing.T) {
	w, fd := pipeFD(t)
	life := AsLongAsAlive(fd)
	go func() {
		time.Sleep(50 * time.Millisecond)
		w.Close() // owner "dies": read end sees EOF
	}()
	var cause error
	err := New().Run(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		cause = context.Cause(ctx)
		return nil
	}, life)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cause == nil || !strings.Contains(cause.Error(), "parent gone") {
		t.Fatalf("context.Cause = %v, want parent gone", cause)
	}
	if !life.Fired() {
		t.Fatal("Fired() = false after a parent death, want true")
	}
}

func TestLifeline_Release_disarms(t *testing.T) {
	w, fd := pipeFD(t)
	defer w.Close()
	life := AsLongAsAlive(fd)

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	cancelled := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- New().Run(ctx, func(ctx context.Context) error {
			close(ready)
			<-ctx.Done()
			close(cancelled)
			return nil
		}, life)
	}()

	<-ready        // Run has armed the Lifeline
	life.Release() // sever the binding at the commit point
	w.Close()      // owner "dies" AFTER release — must be ignored

	select {
	case <-cancelled:
		t.Fatal("body was cancelled after Release — premature death not prevented")
	case <-time.After(2 * reparentInterval):
	}

	cancel() // end Run cleanly ourselves
	if err := <-errCh; err != nil {
		t.Fatalf("err = %v, want nil (Release suppressed the pipe death)", err)
	}
	if life.Fired() {
		t.Fatal("Fired() = true after Release suppressed the pipe death, want false")
	}
}

func TestProcess_Wait(t *testing.T) {
	cases := map[string]struct {
		script   string
		wantCode int
	}{
		"clean":   {"exit 0", 0},
		"nonzero": {"exit 3", 3},
	}
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", c.script)
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			p := Wait(cmd)
			select {
			case <-p.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("Done() did not close after the process exited")
			}
			code := 0
			if err := p.Err(); err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("Err = %v, want *exec.ExitError", err)
				}
				code = ee.ExitCode()
			}
			if code != c.wantCode {
				t.Fatalf("exit code = %d, want %d", code, c.wantCode)
			}
		})
	}
}

func TestReapGroup_exitsOnSIGTERM(t *testing.T) {
	// `exec sleep`: the process BECOMES sleep (no sh wrapper to defer the
	// signal), the single-process shape a real service has — SIGTERM (SIG_DFL)
	// ends it directly.
	pgid, exited := spawnGroup(t, `echo READY; exec sleep 30`)
	start := time.Now()
	if err := ReapGroup(pgid, 2*time.Second, exited); err != nil {
		t.Fatalf("ReapGroup: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v — should exit on SIGTERM well before grace", elapsed)
	}
}

func TestReapGroup_escalatesToSIGKILL(t *testing.T) {
	// Ignores SIGTERM (trap) and never lets a child relay it; only SIGKILL ends it.
	pgid, exited := spawnGroup(t, `trap "" TERM; echo READY; while :; do sleep 0.2; done`)
	grace := 300 * time.Millisecond
	start := time.Now()
	if err := ReapGroup(pgid, grace, exited); err != nil {
		t.Fatalf("ReapGroup: %v", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("took %v — should only die after grace elapsed (%v)", elapsed, grace)
	}
}

// pipeFD returns a write end and a read-end fd owned by the caller-under-test:
// the read end is dup'd into a fresh fd (>= 3, as parentwatch requires) so the
// watcher owns it exclusively, while w drives EOF.
func pipeFD(t *testing.T) (*os.File, zfs.FileDescriptor) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	nfd, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	_ = r.Close()
	return w, zfs.FileDescriptor(nfd)
}

// spawnGroup starts `sh -c script` as its own process-group leader and returns
// its pgid plus a channel closed once it has been waited. The script MUST print
// "READY\n" once it has installed its signal disposition; spawnGroup blocks on
// that marker so a reap never races the trap setup.
func spawnGroup(t *testing.T, script string) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := cmd.Process.Pid // group leader: pgid == pid
	if _, err := io.ReadFull(stdout, make([]byte, len("READY\n"))); err != nil {
		t.Fatalf("waiting for READY: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdout) // drain before Wait (StdoutPipe contract)
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	return pgid, exited
}
