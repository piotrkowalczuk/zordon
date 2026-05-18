package barrier

import (
	"testing"
	"testing/synctest"
	"time"
)

// synctest runs everything inside a "bubble": time is virtual, and the
// runtime only advances time once every goroutine in the bubble is
// blocked. That lets us assert *exact* sequencing of barrier composition
// without sprinkling sleeps and hoping the scheduler does the right
// thing.

// Any must fire exactly when the FIRST source fires — neither sooner
// (early target) nor later (waiter starvation under load). Synctest's
// "advance time only when all goroutines are blocked" property lets us
// read that invariant directly.
//
// We drain `late` at the end because synctest insists every goroutine
// spawned in the bubble has returned before the test function does;
// leaving a goroutine sleeping until 5s would trip the leak check.
func TestAny_firesAtFirstSourceUnderSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := New()
		early, late := New(), New()
		Any(target, early, late)

		// Fire `early` at t+1s, `late` at t+5s.
		go func() { time.Sleep(1 * time.Second); early.Trigger() }()
		go func() { time.Sleep(5 * time.Second); late.Trigger() }()

		start := time.Now()
		<-target.Wait()
		elapsed := time.Since(start)
		if elapsed != 1*time.Second {
			t.Fatalf("target fired at %s, want exactly 1s (first source)", elapsed)
		}
		if late.Triggered() {
			t.Fatal("late source already triggered too — synctest scheduling broke")
		}
		// Drain — let synctest advance virtual time to clean up.
		<-late.Wait()
	})
}

// Wait on Pass() never blocks — a select using Pass as a placeholder
// for "no dep" should proceed immediately on every entry, even mixed
// with a non-fired sibling.
func TestPass_inSelectIsNonBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nonFired := New()
		start := time.Now()
		select {
		case <-Pass().Wait():
			// expected — must not consume any virtual time
		case <-nonFired.Wait():
			t.Fatal("nonFired branch fired ahead of Pass()")
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("select on Pass() advanced virtual clock by %s, want 0", elapsed)
		}
	})
}

// A typical waiter pattern in alpha: a goroutine collects a slice of
// barriers and proceeds only after all have fired. Synctest pins down
// the "all-of" timing: we should unblock exactly at the LATEST source.
func TestBarrier_allOfPattern(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bars := []*Barrier{New(), New(), New()}
		// Fire at 1s, 3s, 7s.
		for i, delay := range []time.Duration{1 * time.Second, 3 * time.Second, 7 * time.Second} {
			b := bars[i]
			d := delay
			go func() { time.Sleep(d); b.Trigger() }()
		}

		start := time.Now()
		for _, b := range bars {
			<-b.Wait()
		}
		elapsed := time.Since(start)
		if elapsed != 7*time.Second {
			t.Fatalf("all-of pattern unblocked at %s, want exactly 7s (latest)", elapsed)
		}
	})
}

// Late observer of a long-past Trigger() must still see the barrier
// closed. This is the property that makes the rest of the system
// race-free: registering as a waiter after-the-fact never misses.
func TestBarrier_lateObserverSeesPastTrigger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New()
		// Trigger immediately…
		b.Trigger()
		// …then advance time and register a waiter.
		time.Sleep(10 * time.Second)
		start := time.Now()
		<-b.Wait()
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("late observer blocked %s, want 0", elapsed)
		}
	})
}

// "first-of" select pattern: waiter selects on a target + a fail
// channel, whichever fires first determines the outcome. This is the
// shape alpha uses to pair every barrier dep with the entity's
// terminal-failure channel.
func TestBarrier_firstOfSelectsCorrectBranch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := New()
		fail := New()
		// fail fires sooner.
		go func() { time.Sleep(2 * time.Second); fail.Trigger() }()
		go func() { time.Sleep(5 * time.Second); target.Trigger() }()

		start := time.Now()
		var via string
		select {
		case <-target.Wait():
			via = "target"
		case <-fail.Wait():
			via = "fail"
		}
		elapsed := time.Since(start)
		if via != "fail" || elapsed != 2*time.Second {
			t.Fatalf("got %s at %s; want fail at 2s", via, elapsed)
		}
		// Drain — let the still-sleeping target goroutine finish before
		// the synctest bubble unwinds.
		<-target.Wait()
	})
}
