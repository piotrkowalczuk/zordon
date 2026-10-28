// Package control owns the unix-domain control socket: typed socket path,
// lifecycle lock, listener, dial helper, and request/response helpers.
package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/daemon"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// AlphaSocketPath is the unix-domain control socket path for one alpha daemon.
// Injecting it once binds all socket and lifecycle-lock operations to methods
// on the value rather than threading a raw path string through every call.
// Implements flag.Value so cmd/alpha registers `--socket` directly on it.
type AlphaSocketPath string

func (p *AlphaSocketPath) String() string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func (p *AlphaSocketPath) Set(s string) error {
	*p = AlphaSocketPath(strings.TrimSpace(s))
	return nil
}

// Path returns the socket path as a string.
func (p AlphaSocketPath) Path() string { return string(p) }

// IsSet reports whether a non-empty socket path was configured.
func (p AlphaSocketPath) IsSet() bool { return p != "" }

// AcquireLock takes an exclusive flock on <p>.lock held for the entire
// lifetime of an alpha daemon (from before orphan reap / socket bind until
// after shutdownAll and handler drain complete).
func (p AlphaSocketPath) AcquireLock(ctx context.Context, onWait func(lockPath string, holderPID int, waited time.Duration)) (func(), error) {
	return p.stateDir().AcquireLock(ctx, p.lockName(), onWait)
}

// TryAcquireLock attempts a non-blocking exclusive flock on <p>.lock.
func (p AlphaSocketPath) TryAcquireLock() (release func(), holderPID int, ok bool, err error) {
	return p.stateDir().TryAcquireLock(p.lockName())
}

// ProbeLock reports whether an alpha daemon currently holds the lifecycle lock
// for p (including during its shutdownAll teardown phase after the control
// socket listener has closed), along with its PID.
func (p AlphaSocketPath) ProbeLock() (held bool, holderPID int) {
	return p.stateDir().ProbeLock(p.lockName())
}

// WaitGone blocks until nothing answers on p, the alpha lifecycle lock
// (<p>.lock) is released, and pid (when > 0) has exited, or until timeout / ctx
// expires. It never signals the process; on timeout the caller exits and leaves
// the running alpha untouched.
func (p AlphaSocketPath) WaitGone(ctx context.Context, pid int, timeout time.Duration, onWait func(holderPID int, waited time.Duration)) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	start := time.Now()
	deadline := start.Add(timeout)
	delay := 20 * time.Millisecond
	var lastReport time.Time

	for {
		c, dialErr := p.Dial(100 * time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
		}
		lockHeld, holderPID := p.ProbeLock()
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
			if holderPID > 0 {
				return fmt.Errorf("old alpha (pid=%d) still running after %s", holderPID, waited.Round(100*time.Millisecond))
			}
			return errors.New("old alpha still running")
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

// Listen binds a unix socket at p with 0600 permissions.
// Stale sockets from a dead previous alpha are removed automatically.
func (p AlphaSocketPath) Listen() (net.Listener, error) {
	if err := p.unlinkStale(); err != nil {
		return nil, err
	}
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)
	ln, err := net.Listen("unix", p.Path())
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", p.Path(), err)
	}
	return ln, nil
}

// Dial tries to connect with a short timeout. Returns ErrNotExist or
// connection-refused-class errors verbatim so the caller can decide to spawn.
func (p AlphaSocketPath) Dial(timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return net.DialTimeout("unix", p.Path(), timeout)
}

// Roundtrip opens one connection, sends req, reads one Response. A bounded
// default deadline (3s, or ctx's deadline when sooner) prevents unary calls
// (OpState, OpShutdown) from hanging forever if alpha is wedged after the
// kernel accepts the unix connection into the listen backlog.
func (p AlphaSocketPath) Roundtrip(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	conn, err := p.Dial(1 * time.Second)
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

// WaitListening polls until p accepts a connection or ctx is done.
func (p AlphaSocketPath) WaitListening(ctx context.Context) error {
	delay := 10 * time.Millisecond
	for {
		c, err := p.Dial(100 * time.Millisecond)
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

const defaultRoundtripTimeout = 3 * time.Second

func (p AlphaSocketPath) stateDir() zfs.StateDir {
	return zfs.StateDir(filepath.Dir(p.Path()))
}

func (p AlphaSocketPath) lockName() string {
	return filepath.Base(p.Path())
}

// unlinkStale removes a socket file if and only if no one is listening on it.
func (p AlphaSocketPath) unlinkStale() error {
	path := p.Path()
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
