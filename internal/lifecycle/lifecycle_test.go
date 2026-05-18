package lifecycle

import "testing"

// Service-shaped lifecycle used across these tests.
//
//	scheduled → running → ready
//	                   → failed
//	ready → stopped
var serviceTestDef = NewDef(
	[]State{"scheduled", "running", "ready", "failed", "stopped"},
	[]Edge{
		{"scheduled", "running"},
		{"running", "ready"},
		{"running", "failed"},
		{"ready", "stopped"},
	},
)

func TestDef_predecessorsAreTransitive(t *testing.T) {
	// Reaching "stopped" implies scheduled, running, ready — fast-paths
	// must not bypass intermediate barriers for observers.
	in := NewInstance(serviceTestDef)
	in.Reach("stopped")
	for _, s := range []State{"scheduled", "running", "ready", "stopped"} {
		if !in.Reached(s) {
			t.Errorf("Reach(stopped) should imply %s, but %s.Reached() == false", s, s)
		}
	}
	if in.Reached("failed") {
		t.Error("Reach(stopped) must not trigger the failed branch")
	}
}

func TestDef_branchesAreIndependent(t *testing.T) {
	// Reach(failed) closes the failure branch and its prefix, but never
	// the success branch.
	in := NewInstance(serviceTestDef)
	in.Reach("failed")
	for _, s := range []State{"scheduled", "running", "failed"} {
		if !in.Reached(s) {
			t.Errorf("Reach(failed) should imply %s", s)
		}
	}
	for _, s := range []State{"ready", "stopped"} {
		if in.Reached(s) {
			t.Errorf("Reach(failed) must not trigger %s", s)
		}
	}
}

func TestDef_reachIsIdempotent(t *testing.T) {
	in := NewInstance(serviceTestDef)
	in.Reach("ready")
	in.Reach("ready") // second call must not panic
	if !in.Reached("ready") {
		t.Fatal("Reach(ready) did not close the ready barrier")
	}
}

func TestDef_lateRegistrationStillSeesPriorTrigger(t *testing.T) {
	// The whole point of barriers being monotonic: a waiter that
	// registers AFTER Reach still sees the closed channel immediately.
	in := NewInstance(serviceTestDef)
	in.Reach("ready")
	// Simulate a late registrant.
	b := in.Barrier("scheduled")
	if b == nil {
		t.Fatal("scheduled barrier missing")
	}
	select {
	case <-b.Wait():
	default:
		t.Fatal("late observer of scheduled saw an open channel after Reach(ready)")
	}
}

func TestDef_unknownStateReachIsNoOp(t *testing.T) {
	in := NewInstance(serviceTestDef)
	in.Reach("not-a-real-state") // must not panic, must not flip anything
	for _, s := range []State{"scheduled", "running", "ready", "failed", "stopped"} {
		if in.Reached(s) {
			t.Errorf("unknown Reach must not trigger %s", s)
		}
	}
}

func TestDef_hasReportsMembership(t *testing.T) {
	if !serviceTestDef.Has("ready") {
		t.Error("Has(ready) == false on serviceTestDef")
	}
	if serviceTestDef.Has("bogus") {
		t.Error("Has(bogus) == true unexpectedly")
	}
}
