package main

import (
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

// handleConfigure registers every serviceCtx up front — before toolchains,
// checkout and build — so the whole build window (minutes for a JVM/Maven
// service) is served by a ctx whose process does not exist yet. Any OpState
// landing there used to dereference the nil *exec.Cmd and panic the handler
// goroutine, which took alpha and the in-flight `zordon start` down with it.
func TestAlphaState_snapshot_beforeSpawn(t *testing.T) {
	s := &alphaState{}
	s.addService(newServiceCtx("petclinic", "java"))

	info := s.snapshot()

	if len(info.Running) != 1 {
		t.Fatalf("Running=%d, want 1", len(info.Running))
	}
	got := info.Running[0]
	if got.Name != "petclinic" {
		t.Errorf("Name=%q, want %q", got.Name, "petclinic")
	}
	if got.PID != 0 {
		t.Errorf("PID=%d, want 0 for a service that has not been spawned", got.PID)
	}
	if got.Readiness != "" {
		t.Errorf("Readiness=%q, want empty before spawn", got.Readiness)
	}
}

func TestAlphaState_snapshot_afterSpawn(t *testing.T) {
	s := &alphaState{}
	s.addService(newServiceCtx("api", "go"))
	s.setServicePID("api", 4242)
	s.setReadiness("api", protocol.ReadinessReady)

	info := s.snapshot()

	if len(info.Running) != 1 {
		t.Fatalf("Running=%d, want 1", len(info.Running))
	}
	got := info.Running[0]
	if got.PID != 4242 {
		t.Errorf("PID=%d, want 4242", got.PID)
	}
	if got.Readiness != protocol.ReadinessReady {
		t.Errorf("Readiness=%q, want %q", got.Readiness, protocol.ReadinessReady)
	}
}

// setServicePID must tolerate a service that stopServices already removed:
// the supervisor publishes its pid without holding the map open.
func TestAlphaState_setServicePID_unknownService(t *testing.T) {
	s := &alphaState{}
	s.setServicePID("gone", 1)

	if n := len(s.snapshot().Running); n != 0 {
		t.Fatalf("Running=%d, want 0", n)
	}
}
