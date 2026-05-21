// Package parentwatch detects death of a spawning ("parent") process
// from the child side using mechanisms that do not require the parent
// to run any cleanup code. SIGKILL and OOM-kill are explicitly covered:
// the kernel is the only actor we rely on.
//
// Two detectors, both unconditional on parent cooperation:
//
//   - Pipe-EOF (primary). The parent calls Pipe() to get a pair, hands
//     the read end to the child via ExtraFiles (Attach), and keeps the
//     write end alive for as long as it wants the child to consider it
//     alive. When the parent process table entry goes away — for ANY
//     reason, SIGKILL/OOM included — the kernel closes its write end,
//     so a blocking Read on the child's read end returns EOF
//     immediately. The only detector that is both instant and
//     SIGKILL/OOM-proof on darwin (no PR_SET_PDEATHSIG).
//
//   - getppid() poll. When the child's direct parent dies, the kernel
//     reparents the child to launchd/init (pid 1), so a changed ppid
//     means the spawner is gone. Works with no fd plumbing at all;
//     useful as a safety net when the pipe fd is not (or cannot be)
//     passed.
//
// Two consumers in this repo, different needs:
//
//   - tommy lives only as long as alpha and never wants to "detach"
//     from its spawner. It uses WatchForever, which arms both
//     detectors and never stops on its own.
//
//   - alpha is supervised by zordon ONLY until it signals READY. After
//     that point zordon may exit cleanly and alpha must keep running.
//     Alpha uses Watch, which arms pipe-EOF only and exposes Stop()
//     so the watch can be dropped once READY is signalled.
//
// Conventions:
//
//   - Read-end fd is always inherited via os/exec.Cmd.ExtraFiles and
//     announced via an env var the consumer chooses (e.g.
//     TOMMY_PARENT_FD, ZORDON_PARENT_FD). The child reads that env
//     var, opens fd via os.NewFile, and immediately marks it
//     close-on-exec so the fd does NOT leak into any process the
//     child itself exec()s afterwards.
//
//   - Call Watch / WatchForever as early as possible in main, ideally
//     before any other code that might call os.OpenFile or similar.
//     Otherwise an earlier open could be assigned the very fd the
//     parent passed, and parentwatch would attach to the wrong file.
package parentwatch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

// Pipe returns a fresh pipe pair suitable for parent-death detection.
// The parent passes r to a child (via Attach) and keeps w open for its
// own lifetime; the child sees EOF on r the instant the kernel closes
// every write end, which happens on ANY parent exit, signalled or not.
func Pipe() (r, w *os.File, err error) {
	return os.Pipe()
}

// Attach wires r into cmd as an extra inherited fd and tells the child
// where to find it. The child sees the fd as 3+len(prevExtraFiles); we
// append envVar=<fd> to cmd.Env. The caller still owns r and w — Attach
// does NOT close anything.
func Attach(cmd *exec.Cmd, r *os.File, envVar string) {
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, r)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", envVar, fd))
}

// config is the internal struct shaped by Option values.
type config struct {
	pollInterval time.Duration
	errorLog     func(error)
}

// Option tunes Watch/WatchForever behavior.
type Option func(*config)

// WithPollInterval sets the getppid() poll interval for WatchForever.
// Default is 250ms (low cost, prompt detection). Ignored by Watch
// (which has no getppid poll).
func WithPollInterval(d time.Duration) Option {
	return func(c *config) { c.pollInterval = d }
}

// WithErrorLog installs a sink for non-EOF Read errors on the parent
// fd. Such errors (EIO, EBADF, ...) are treated as death (matching
// tommy's historical behavior) but are otherwise silently swallowed
// without a sink. Use this to plug in your structured logger.
func WithErrorLog(fn func(error)) Option {
	return func(c *config) { c.errorLog = fn }
}

func newConfig(opts []Option) *config {
	c := &config{pollInterval: 250 * time.Millisecond}
	for _, o := range opts {
		o(c)
	}
	return c
}

// openInheritedFD validates fd, marks it close-on-exec, switches it to
// non-blocking I/O, and wraps it in an *os.File. fd <= 0 ⇒ (nil, nil)
// so callers can treat "unsupervised" as a graceful no-op. The caller
// resolves fd at startup (from a flag or its env-mapped equivalent) —
// parentwatch never reads the environment itself.
func openInheritedFD(fd zfs.FileDescriptor) (*os.File, error) {
	if fd <= 0 {
		return nil, nil
	}
	// fd 0/1/2 are stdio; the parent uses ExtraFiles which starts at 3.
	// Anything lower means something went wrong on the spawn side and
	// we would otherwise hijack a stdio stream via os.NewFile.
	if fd < 3 {
		return nil, fmt.Errorf("parentwatch: fd=%d below first ExtraFiles slot", fd)
	}
	// CloseOnExec drops the fd from any process this child itself
	// exec()s. Without it the parent's pipe leaks into grandchildren —
	// which both confuses semantics ("who is your parent?") and risks
	// fd leaks across long chains.
	syscall.CloseOnExec(int(fd))
	// Mark the fd O_NONBLOCK BEFORE os.NewFile so Go's runtime adopts
	// it via the netpoller. Without this, Read() goes through a raw
	// blocking syscall.Read; the kernel doesn't wake that read when
	// another goroutine closes the fd, so Stop() (which closes the fd
	// then waits for the read goroutine) hangs forever. With the fd
	// poller-registered, os.File.Close interrupts the pending Read
	// with ErrClosed — the event-driven shutdown Stop relies on. No
	// behavior change on the death path: EOF still arrives the same way.
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		return nil, fmt.Errorf("parentwatch: SetNonblock fd %d: %w", fd, err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("parentwatch:fd%d", fd))
	if f == nil {
		return nil, fmt.Errorf("parentwatch: fd %d not open", fd)
	}
	return f, nil
}

// Watcher arms the pipe-EOF detector against a single inherited fd.
// Construct via Watch. After construction the consumer waits on Died()
// (a closed channel means the parent is gone) and calls Stop() exactly
// once when the watch is no longer needed (e.g. after the consumer has
// signalled READY upstream and the parent is allowed to exit cleanly).
//
// Stop is idempotent and races safely against death:
//
//   - if Stop wins, the goroutine sees the fd close and exits silently;
//     Died() stays open forever.
//   - if death wins, Died() is already closed and Stop is a no-op.
//
// The "stopped" flag is set BEFORE the fd is closed so that the Read
// error caused by Stop itself can never be misinterpreted as parent
// death (which would otherwise be a spurious Died() right at clean
// shutdown — a race we explicitly do not want).
type Watcher struct {
	f       *os.File
	cfg     *config
	died    chan struct{}
	done    chan struct{}
	stopped atomic.Bool
	once    sync.Once
}

// Watch arms a pipe-EOF watcher against fd. fd <= 0 ⇒ returns (nil,
// nil), meaning "no parent supervises us"; the consumer should handle
// a nil *Watcher as "skip the watch". Callers obtain fd from a typed
// flag (auto-mapped to e.g. ZORDON_PARENT_FD); this package no longer
// reads the environment itself.
func Watch(fd zfs.FileDescriptor, opts ...Option) (*Watcher, error) {
	f, err := openInheritedFD(fd)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, nil
	}
	w := &Watcher{
		f:    f,
		cfg:  newConfig(opts),
		died: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// Died returns a channel that is closed iff the parent process is gone
// (pipe-EOF observed) AND Stop has not been called first. A nil
// *Watcher returns a nil channel — selecting on it blocks forever,
// which is the right behavior for "no supervision configured".
func (w *Watcher) Died() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.died
}

// Stop releases the parent fd and joins the watch goroutine. After
// Stop returns, Died() will not be closed retroactively — a Stop+death
// race resolves to Stop. Idempotent and safe from any goroutine.
// No-op on a nil *Watcher.
func (w *Watcher) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		// Order matters: flag BEFORE close so the watch goroutine, on
		// observing the Read error caused by our close, sees stopped
		// already true and does NOT close died.
		w.stopped.Store(true)
		_ = w.f.Close()
		<-w.done
	})
}

func (w *Watcher) run() {
	defer close(w.done)
	// A blocking pipe Read only returns when data arrives or every
	// write end is closed; we hold no write end. So the only wakeup
	// path is the parent's last write end closing — which the kernel
	// does on any parent exit, SIGKILL/OOM included. Stray data
	// (a misbehaving parent writing into the fd) is ignored; only an
	// error/EOF counts as a death signal.
	buf := make([]byte, 64)
	for {
		_, err := w.f.Read(buf)
		if err == nil {
			continue
		}
		if w.stopped.Load() {
			// Stop closed the fd; we're winding down cleanly.
			return
		}
		if !errors.Is(err, io.EOF) && w.cfg.errorLog != nil {
			w.cfg.errorLog(err)
		}
		// Either EOF (clean parent death) or some other Read error —
		// both treated as "parent is gone" per tommy's historical
		// behavior.
		close(w.died)
		return
	}
}

// WatchForever arms BOTH detectors (pipe-EOF + getppid() poll) and
// returns a single channel that closes when either fires. There is no
// Stop: this is for consumers (tommy) that must die with their parent
// no matter what, and have no notion of "detaching".
//
// envVar may be unset; in that case only the getppid() poll is armed.
// origPPID is captured AT CALL TIME — call WatchForever early so the
// snapshot reflects the spawner, not whatever the child re-parents to
// later. Like Watch, fd <= 0 means "no pipe", but the getppid reparent
// detector is always armed.
func WatchForever(fd zfs.FileDescriptor, opts ...Option) (<-chan struct{}, error) {
	cfg := newConfig(opts)
	f, err := openInheritedFD(fd)
	if err != nil {
		return nil, err
	}

	gone := make(chan struct{})
	var once sync.Once
	trigger := func() { once.Do(func() { close(gone) }) }

	if f != nil {
		go watchPipeForever(f, trigger, cfg.errorLog)
	}
	go watchReparent(os.Getppid(), cfg.pollInterval, trigger)
	return gone, nil
}

func watchPipeForever(f *os.File, trigger func(), errorLog func(error)) {
	buf := make([]byte, 64)
	for {
		_, err := f.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) && errorLog != nil {
				errorLog(err)
			}
			trigger()
			return
		}
	}
}

func watchReparent(origPPID int, interval time.Duration, trigger func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		if os.Getppid() != origPPID {
			trigger()
			return
		}
	}
}

// IgnoreSIGPIPE drains SIGPIPE into a buffered channel so writes to a
// closed pipe (e.g. a stdout the spawner held) turn into EPIPE errors
// instead of killing the process. Common pairing with parent-death
// watching: once the spawner dies its stdout reader vanishes, and the
// next log line on the inherited stdout would otherwise SIGPIPE-kill
// us before the watchdog could react.
//
// Implementation: signal.Notify with a discarded channel is equivalent
// to SIG_IGN for our purposes (signals are silently dropped). We avoid
// signal.Ignore because that installs SIG_IGN, which exec() preserves
// — undesirable for any process that later exec()s another binary.
func IgnoreSIGPIPE() {
	// We deliberately don't return the channel: nothing reads it.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}
