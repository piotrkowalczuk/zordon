package lifecycle

import (
	"testing"
	"testing/synctest"
	"time"
)

// Service-shaped lifecycle reused across these synctest scenarios.
var svcDef = NewDef(
	[]State{"scheduled", "running", "ready", "failed", "stopped"},
	[]Edge{
		{"scheduled", "running"},
		{"running", "ready"},
		{"running", "failed"},
		{"ready", "stopped"},
	},
)

// A waiter that registers AFTER a fast-path Reach must still see every
// predecessor barrier closed. With virtual time we can prove the
// "scheduled" observation is instantaneous even though it happened
// in the past.
func TestInstance_observerOfPastStateNeverBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := NewInstance(svcDef)
		go func() {
			time.Sleep(100 * time.Millisecond)
			in.Reach("ready") // closes scheduled, running, ready in one go
		}()

		// Block until ready, then check that scheduled is observable
		// instantly with no virtual time passing.
		<-in.Barrier("ready").Wait()
		start := time.Now()
		<-in.Barrier("scheduled").Wait()
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("observing past 'scheduled' took %s; must be 0", elapsed)
		}
	})
}

// A typical orchestration: one goroutine drives lifecycle transitions
// over time; another waits for a SPECIFIC state and proceeds. Synctest
// guarantees the waiter unblocks at exactly the moment the producer
// reaches it — no scheduler luck involved.
func TestInstance_waiterFiresAtExpectedState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := NewInstance(svcDef)

		// Producer: scheduled at 1s, running at 2s, ready at 3s.
		go func() {
			time.Sleep(1 * time.Second)
			in.Reach("scheduled")
			time.Sleep(1 * time.Second)
			in.Reach("running")
			time.Sleep(1 * time.Second)
			in.Reach("ready")
		}()

		start := time.Now()
		<-in.Barrier("running").Wait()
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("'running' observed at %s, want 2s", elapsed)
		}
		// Continue the test: 'ready' must observe at exactly 3s total.
		<-in.Barrier("ready").Wait()
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("'ready' observed at %s, want 3s", elapsed)
		}
	})
}

// Mutually exclusive branches: reaching the failure branch must NOT
// close the success branch's barrier, even when an observer is waiting
// on it. Without virtual time this'd be untestable — a deadlocked
// waiter looks identical to one that's just slow.
func TestInstance_branchBarriersStayDistinct(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := NewInstance(svcDef)

		go func() {
			time.Sleep(500 * time.Millisecond)
			in.Reach("failed")
		}()

		// Wait for 'failed' to fire, then assert 'ready' is still open.
		<-in.Barrier("failed").Wait()
		if in.Reached("ready") {
			t.Fatal("ready barrier triggered by failed-branch Reach")
		}
		if in.Reached("stopped") {
			t.Fatal("stopped barrier triggered by failed-branch Reach")
		}
		// Predecessors (scheduled, running) should have closed.
		if !in.Reached("scheduled") || !in.Reached("running") {
			t.Fatal("Reach(failed) must close scheduled+running predecessors")
		}
	})
}

// ReachedAt stamps each state with the virtual time it was reached, and
// reports not-reached for a pending or unknown state.
func TestInstance_reachedAtRecordsTransitionTimes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := NewInstance(svcDef)
		start := time.Now()

		if _, ok := in.ReachedAt("scheduled"); ok {
			t.Fatal("ReachedAt(scheduled) reported reached before any Reach")
		}
		if _, ok := in.ReachedAt("nope"); ok {
			t.Fatal("ReachedAt(unknown) must report not-reached")
		}

		time.Sleep(1 * time.Second)
		in.Reach("scheduled")
		time.Sleep(1 * time.Second)
		in.Reach("running")
		time.Sleep(1 * time.Second)
		in.Reach("ready")

		for _, c := range []struct {
			state State
			want  time.Duration
		}{
			{"scheduled", 1 * time.Second},
			{"running", 2 * time.Second},
			{"ready", 3 * time.Second},
		} {
			at, ok := in.ReachedAt(c.state)
			if !ok {
				t.Fatalf("ReachedAt(%q): not reached", c.state)
			}
			if got := at.Sub(start); got != c.want {
				t.Errorf("ReachedAt(%q) = +%s, want +%s", c.state, got, c.want)
			}
		}
	})
}

// A state skipped by a fast-path Reach of a later state is stamped with
// that later Reach's time, not left unset — so a phase span computed from
// it is never garbage.
func TestInstance_reachedAtStampsSkippedPredecessors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := NewInstance(svcDef)
		start := time.Now()

		time.Sleep(2 * time.Second)
		in.Reach("ready") // scheduled+running never reached explicitly

		for _, s := range []State{"scheduled", "running", "ready"} {
			at, ok := in.ReachedAt(s)
			if !ok {
				t.Fatalf("ReachedAt(%q): predecessor not stamped by fast-path Reach", s)
			}
			if got := at.Sub(start); got != 2*time.Second {
				t.Errorf("ReachedAt(%q) = +%s, want +2s (later Reach time)", s, got)
			}
		}
	})
}

// "Race" between two transitions in a fast-paced producer. The waiter
// observes the FINAL state and ALL intermediates monotonically — even
// when transitions happen back-to-back within the same virtual instant.
func TestInstance_backToBackTransitionsAllObservable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := NewInstance(svcDef)

		go func() {
			// No sleeps — burn through every state at virtual t=0.
			in.Reach("scheduled")
			in.Reach("running")
			in.Reach("ready")
			in.Reach("stopped")
		}()

		// Wait for the LAST state and then verify every prior state is
		// reachable in the observer's slow-path checks.
		<-in.Barrier("stopped").Wait()
		for _, s := range []State{"scheduled", "running", "ready", "stopped"} {
			if !in.Reached(s) {
				t.Errorf("expected %q reached after fast-path Reach(stopped)", s)
			}
		}
	})
}
