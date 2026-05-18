// Package barrier is a one-shot edge primitive: a channel that closes
// when the moment is reached, with cheap fan-in composition.
//
// Why a type and not a plain chan struct{}? Two reasons:
//
//   - Trigger() is idempotent via sync.Once. Closing a closed channel
//     panics, so callers either need their own guard or use this.
//   - Any() composes "first of N" without reflection or fiddly
//     goroutine bookkeeping at the call site.
//
// A Barrier is always either pending or triggered (and once triggered,
// stays triggered forever). That monotonicity is what makes the rest of
// the system race-free: late observers of a long-past Trigger() still
// see the channel as closed.
package barrier

import "sync"

// Barrier is a one-shot edge. Construct with New, fire with Trigger,
// observe with Wait or Triggered. Zero value is NOT usable — always go
// through New.
type Barrier struct {
	ch   chan struct{}
	once sync.Once
}

// New returns a fresh, untriggered Barrier.
func New() *Barrier { return &Barrier{ch: make(chan struct{})} }

// Trigger fires the barrier. Idempotent: any number of calls beyond
// the first are no-ops.
func (b *Barrier) Trigger() { b.once.Do(func() { close(b.ch) }) }

// Wait returns a channel that closes when the barrier fires (or is
// already closed if it has already fired). Safe to call any number of
// times; the same channel is returned each time.
func (b *Barrier) Wait() <-chan struct{} { return b.ch }

// Triggered reports whether the barrier has fired. Non-blocking.
func (b *Barrier) Triggered() bool {
	select {
	case <-b.ch:
		return true
	default:
		return false
	}
}

// Any wires target to fire as soon as any of sources fires. One
// goroutine per source — each does a single receive then exits, so the
// scheduling cost is O(sources) but constant per source and bounded by
// the source set's lifetime.
//
// Idempotent target: extra firings after the first are absorbed by
// target's sync.Once.
func Any(target *Barrier, sources ...*Barrier) {
	for _, s := range sources {
		go func(s *Barrier) {
			<-s.Wait()
			target.Trigger()
		}(s)
	}
}

// passed is the singleton "already-fired" barrier returned by Pass.
var passed = func() *Barrier { b := New(); b.Trigger(); return b }()

// Pass returns a singleton, pre-triggered Barrier. Use it where "no
// dependency" needs to be expressed as "one always-satisfied dependency"
// so the consumer doesn't need a special case for the empty-deps path.
// All callers share the same underlying chan, which is fine because
// closed channels are read-only from the receive side.
func Pass() *Barrier { return passed }
