// Package control owns the unix-domain control socket: deterministic path
// derivation, dial helper (with optional autospawn left to the caller),
// and a tiny request/response loop.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

const socketPrefix = "zordon-alpha"

// SocketPath derives a deterministic socket path from the absolute Alphasfile
// path. Both zordon and alpha compute it the same way, so no shared state
// file is needed.
//
// Lives under $XDG_RUNTIME_DIR if available (Linux), else os.TempDir().
func SocketPath(alphasfilePath string) (string, error) {
	abs, err := filepath.Abs(alphasfilePath)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}
	sum := sha256.Sum256([]byte(abs))
	name := fmt.Sprintf("%s-%s.sock", socketPrefix, hex.EncodeToString(sum[:8]))
	return filepath.Join(socketDir(), name), nil
}

// StateDir returns a deterministic per-Alphasfile directory under the system
// temp dir, used for generated files (file{} blocks, alpha logs, etc.).
// Mkdir is on-demand; this function only returns the path.
func StateDir(alphasfilePath string) (string, error) {
	abs, err := filepath.Abs(alphasfilePath)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}
	sum := sha256.Sum256([]byte(abs))
	name := fmt.Sprintf("zordon-%s", hex.EncodeToString(sum[:8]))
	return filepath.Join(os.TempDir(), name), nil
}

func socketDir() string {
	if runtime.GOOS == "linux" {
		if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
			return d
		}
	}
	return os.TempDir()
}

// Lock takes an exclusive, blocking flock on <stateDir>/start.lock so that
// only one `zordon start` operates on a given Alphasfile at a time. The
// returned func releases the lock (and must be called). stateDir is created
// if missing.
//
// Federation acquires locks strictly top-down (root → invocation), a
// consistent global order, so concurrent zordon invocations can't deadlock.
func Lock(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	lockPath := filepath.Join(stateDir, "start.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
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

// unlinkStale removes a socket file if and only if no one is listening on it.
func unlinkStale(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		c.Close()
		return fmt.Errorf("%s is already in use by another alpha", path)
	}
	return os.Remove(path)
}

// Dial tries to connect with a short timeout. Returns os.ErrNotExist or
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
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

// Roundtrip opens one connection, sends req, reads one Response.
func Roundtrip(ctx context.Context, path string, req *protocol.Request) (*protocol.Response, error) {
	conn, err := Dial(path, 1*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

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
			c.Close()
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
