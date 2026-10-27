// Package control owns the unix-domain control socket: deterministic path
// derivation, dial helper (with optional autospawn left to the caller),
// and a tiny request/response loop.
package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"syscall"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/daemon"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Socket/state paths are now derived per-invocation by internal/invocation
// (Invocation.SocketPath / .StateDir), so this package only owns the lock,
// the listener, and the request/response helpers.

// Lock takes an exclusive, blocking flock on <stateDir>/start.lock so that
// only one `zordon start` operates on a given Alphasfile at a time. The
// returned func releases the lock (and must be called). stateDir is created
// if missing.
//
// Federation acquires locks strictly top-down (root → invocation), a
// consistent global order, so concurrent zordon invocations can't deadlock.
func Lock(stateDir string) (func(), error) {
	return zfs.AcquireLock(stateDir, "start")
}

// LockWait is Lock with context cancellation and contention reporting via onWait.
func LockWait(ctx context.Context, stateDir string, onWait func(lockPath string, holderPID int, waited time.Duration)) (func(), error) {
	return zfs.AcquireLockWait(ctx, stateDir, "start", onWait)
}

// LockAlpha takes an exclusive flock on <sockPath>.lock held for the entire
// lifetime of an alpha daemon (from before orphan reap / socket bind until
// after shutdownAll and handler drain complete).
func LockAlpha(ctx context.Context, sockPath string, onWait func(lockPath string, holderPID int, waited time.Duration)) (func(), error) {
	return zfs.AcquireLockWait(ctx, filepath.Dir(sockPath), filepath.Base(sockPath), onWait)
}

// ProbeAlpha reports whether an alpha daemon currently holds the lifecycle
// lock for sockPath (including during its shutdownAll teardown phase after the
// control socket listener has closed), along with its PID.
func ProbeAlpha(sockPath string) (held bool, holderPID int) {
	return zfs.ProbeLock(filepath.Dir(sockPath), filepath.Base(sockPath))
}

// TerminateAlpha sends SIGTERM to an alpha process by PID.
func TerminateAlpha(pid int) error {
	return daemon.Terminate(pid)
}

// WaitAlphaGone blocks until nothing answers on sockPath, the alpha lifecycle
// lock (<sockPath>.lock) is released, and pid (when > 0) has exited. If the old
// alpha has not finished tearing down within timeout, WaitAlphaGone escalates
// to SIGKILL (calling onForceKill when non-nil) and waits up to 2s for the
// kernel to release its lock and reap the process.
func WaitAlphaGone(ctx context.Context, sockPath string, pid int, timeout time.Duration, onWait func(holderPID int, waited time.Duration), onForceKill func(pid int)) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	start := time.Now()
	deadline := start.Add(timeout)
	delay := 20 * time.Millisecond
	var lastReport time.Time
	killed := false

	for {
		c, dialErr := Dial(sockPath, 100*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
		}
		lockHeld, holderPID := ProbeAlpha(sockPath)
		if holderPID == 0 {
			holderPID = pid
		}
		pidAlive := pid > 0 && daemon.Alive(pid)
		if dialErr != nil && !lockHeld && !pidAlive {
			return nil
		}

		waited := time.Since(start)
		if onWait != nil && waited >= 1*time.Second && (lastReport.IsZero() || time.Since(lastReport) >= 5*time.Second) {
			onWait(holderPID, waited)
			lastReport = time.Now()
		}

		if time.Now().After(deadline) {
			if !killed && holderPID > 0 {
				if onForceKill != nil {
					onForceKill(holderPID)
				}
				_ = daemon.Kill(holderPID)
				killed = true
				deadline = time.Now().Add(2 * time.Second)
			} else {
				if holderPID > 0 {
					return fmt.Errorf("old alpha (pid=%d) still running after %s", holderPID, waited.Round(100*time.Millisecond))
				}
				return errors.New("old alpha still running")
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
}

// Listen binds a unix socket at path with 0600 permissions.
// Stale sockets from a dead previous alpha are removed automatically.
func Listen(path string) (net.Listener, error) {
	if err := unlinkStale(path); err != nil {
		return nil, err
	}
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	return ln, nil
}

// Dial tries to connect with a short timeout. Returns ErrNotExist or
// connection-refused-class errors verbatim so the caller can decide to spawn.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return net.DialTimeout("unix", path, timeout)
}

// IsNotRunning reports whether err means "no alpha is listening on this path".
func IsNotRunning(err error) bool {
	if err == nil {
		return false
	}
	if zfs.IsMissingErr(err) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// Roundtrip opens one connection, sends req, reads one Response. A bounded
// default deadline (3s, or ctx's deadline when sooner) prevents unary calls
// (OpState, OpShutdown) from hanging forever if alpha is wedged after the
// kernel accepts the unix connection into the listen backlog.
func Roundtrip(ctx context.Context, path string, req *protocol.Request) (*protocol.Response, error) {
	conn, err := Dial(path, 1*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(defaultRoundtripTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	if err := protocol.NewEncoder(conn).Write(req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	var resp protocol.Response
	if err := protocol.NewDecoder(conn).Read(&resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	return &resp, nil
}

// WaitListening polls until path accepts a connection or ctx is done.
func WaitListening(ctx context.Context, path string) error {
	delay := 10 * time.Millisecond
	for {
		c, err := Dial(path, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
}

const defaultRoundtripTimeout = 3 * time.Second

// unlinkStale removes a socket file if and only if no one is listening on it.
func unlinkStale(path string) error {
	if zfs.Missing(path) {
		return nil
	}
	if _, err := zfs.Stat(path); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("%s is already in use by another alpha", path)
	}
	return zfs.RemoveIfPresent(path)
}
