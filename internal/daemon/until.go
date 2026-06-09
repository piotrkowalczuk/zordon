package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/parentwatch"
	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// reparentInterval is how often a Lifeline polls getppid() as a backup to the
// pipe-EOF detector.
const reparentInterval = 250 * time.Millisecond

// Until is the external stop condition layered on Run's universal sources
// (terminal signal, parent ctx). arm starts watching and calls cancel(cause)
// when the condition fires; it returns a stop func that tears the watch down
// (Run defers it), or an error if arming fails.
type Until interface {
	arm(cancel context.CancelCauseFunc) (stop func(), err error)
}

// Forever couples the daemon to nothing external: only a terminal signal or the
// parent ctx ends it. Use for a root / standalone process.
func Forever() Until { return forever{} }

type forever struct{}

func (forever) arm(context.CancelCauseFunc) (func(), error) { return func() {}, nil }

// Lifeline binds the daemon's life to the process that owns fd: when that
// process dies — pipe-EOF (instant, SIGKILL/OOM-proof) or a getppid() reparent
// as backup — it cancels the daemon's context with that cause. Release severs
// the binding (the commit point) so the owner may afterwards exit cleanly
// without ending the daemon.
//
// Release must precede the body telling the owner it may exit (e.g. sending an
// OK): Stop sets its guard before closing the fd, so once Release returns no
// death can fire; and the owner cannot exit before it reads OK, so the ordering
// is race-free with no extra flag.
type Lifeline struct {
	fd zfs.FileDescriptor

	tripped atomic.Bool // a detector fired (parent died) before any Release

	mu       sync.Mutex
	released bool
	disarm   func()
}

// AsLongAsAlive builds a Lifeline against fd (an inherited parent-death pipe).
// fd <= 0 arms the getppid() poll only (standalone-safe).
func AsLongAsAlive(fd zfs.FileDescriptor) *Lifeline { return &Lifeline{fd: fd} }

func (l *Lifeline) arm(cancel context.CancelCauseFunc) (func(), error) {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return func() {}, nil // released before armed → never fires
	}
	l.mu.Unlock()

	// Pipe-EOF leg (primary). nil watcher ⇒ fd<=0 (getppid-only); a non-nil
	// error is a misconfigured fd and is fatal to arming.
	w, err := parentwatch.Watch(l.fd)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	var fireOnce sync.Once
	fire := func(reason string) {
		fireOnce.Do(func() {
			l.tripped.Store(true)
			cancel(errors.New(reason))
		})
	}

	if w != nil {
		go func() {
			select {
			case <-w.Died():
				fire("parent gone (pipe EOF)")
			case <-done:
			}
		}()
	}

	// getppid() reparent backup: on parent death the kernel reparents us to
	// launchd/init, so a changed ppid means the spawner is gone.
	origPPID := os.Getppid()
	go func() {
		t := time.NewTicker(reparentInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if os.Getppid() != origPPID {
					fire("parent gone (reparented)")
					return
				}
			case <-done:
				return
			}
		}
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			if w != nil {
				w.Stop()
			}
		})
	}

	l.mu.Lock()
	l.disarm = stop
	l.mu.Unlock()
	return stop, nil
}

// Fired reports whether a detector tripped (the owner died) before any Release.
// After Release on a clean handshake it stays false, so a caller woken by Run's
// context can tell a parent death apart from a signal.
func (l *Lifeline) Fired() bool { return l.tripped.Load() }

// Release severs the binding. Idempotent and safe from any goroutine. A Release
// before the Lifeline is armed marks it so arm() never fires.
func (l *Lifeline) Release() {
	l.mu.Lock()
	l.released = true
	disarm := l.disarm
	l.mu.Unlock()
	if disarm != nil {
		disarm()
	}
}
