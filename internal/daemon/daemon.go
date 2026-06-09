// Package daemon is the process-lifecycle skeleton shared by alpha and tommy.
//
// It runs ONE body under a context that is cancelled the instant any "koniec"
// (the end) source fires: the body returning, an upstream process dying (an
// Until), a terminal signal, or the parent context. It carries no business
// logic — the kernel-level liveness detectors live in internal/parentwatch,
// which this package composes; reaping is the body's job via ReapGroup.
//
// Two correctness invariants motivate the shape:
//
//   - no orphans — a supervised child never outlives the daemon (the body
//     reaps on ctx cancel; tommy's whole purpose);
//   - no premature death — a daemon survives its spawner's clean exit after a
//     handshake (the spawner edge is an Until that can be Released at the
//     commit point).
//
// The "why" of a shutdown is carried by context.Cause(ctx) — the signal name,
// the Until's reason, or the parent's cause — so there is no separate cause
// type to return.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/piotrkowalczuk/zordon/internal/parentwatch"
)

// defaultSignals are the terminal signals a daemon ends on when New is given
// none — the common minimum every daemon honors.
var defaultSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}

// Daemon runs a single body under a shared, cancelable lifetime, ending on the
// signals it was constructed with.
type Daemon struct {
	signals []os.Signal
}

// New builds a Daemon that ends on the given terminal signals (default:
// SIGTERM, SIGINT). tommy passes a wider set because it shares the service's
// process group and must not be dropped by a group-wide SIGHUP/SIGQUIT before
// it reaps; alpha keeps the default. The signals are always CAUGHT
// (signal.Notify), never Ignored: signal.Ignore installs SIG_IGN, which exec()
// preserves into a child the body spawns — the child would then ignore the
// signal too. A handler is reset to SIG_DFL across exec, so the child keeps
// normal signal behavior.
func New(signals ...os.Signal) *Daemon {
	if len(signals) == 0 {
		signals = defaultSignals
	}
	return &Daemon{signals: signals}
}

// Run executes fn under a context cancelled at the first koniec: a terminal
// signal, until firing (an upstream death), or the parent ctx. fn observes the
// cancellation via ctx.Done() and winds down; context.Cause(ctx) reports WHY.
// Run returns fn's error. SIGPIPE is drained and signals armed BEFORE fn starts,
// so any process fn exec()s inherits the correct dispositions.
func (d *Daemon) Run(ctx context.Context, fn func(context.Context) error, until Until) error {
	// Once the spawner dies its stdout reader vanishes; the next log line on the
	// inherited stdout would otherwise SIGPIPE-kill us before we could react.
	parentwatch.IgnoreSIGPIPE()

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, d.signals...)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case s := <-sigCh:
			cancel(fmt.Errorf("signal: %s", s))
		case <-runCtx.Done():
		}
	}()

	stop, err := until.arm(cancel)
	if err != nil {
		return err
	}
	defer stop()

	// Pass the body's result straight through: nil for a clean wind-down, its
	// error otherwise. The "why" of a cancellation rides context.Cause for the
	// body that needs to branch on it (alpha logs it / tells parent-death from
	// a signal).
	return fn(runCtx)
}
