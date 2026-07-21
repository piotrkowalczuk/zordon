package main

import (
	"net"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/probe"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

// The probe here points at a port nothing is listening on, so any state that
// renders without contacting it proves `status` did not probe: a live check
// would have come back "connection refused" instead.
func TestServiceState(t *testing.T) {
	cases := map[string]struct {
		status protocol.ServiceStatus
		want   string
	}{
		"not spawned yet":   {protocol.ServiceStatus{}, "starting"},
		"probing":           {protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessProbing}, "running pid=7 [probing]"},
		"failed":            {protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessFailed}, "failed"},
		"unknown readiness": {protocol.ServiceStatus{PID: 7, Readiness: "wat"}, "starting"},
	}

	svc := serviceWithProbe(t)
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := serviceState(t.Context(), svc, c.status); got != c.want {
				t.Fatalf("serviceState() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestServiceState_readyProbesLive(t *testing.T) {
	status := protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessReady}

	got := serviceState(t.Context(), serviceWithProbe(t), status)

	want := "running pid=7 [unhealthy: "
	if !strings.HasPrefix(got, want) {
		t.Fatalf("serviceState() = %q, want it to start with %q", got, want)
	}
}

// A service that declares no readiness is called ready by alpha on
// stabilization alone; there is nothing to check, and reaching for
// Runtime.Readiness must not trip over a nil Runtime (build-only service).
func TestServiceState_noReadiness(t *testing.T) {
	cases := map[string]*alphasfile.Service{
		"no runtime block": {Toolchain: "go"},
		"no readiness":     {Toolchain: "go", Runtime: &alphasfile.RuntimeConfig{Name: "app"}},
	}

	status := protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessReady}
	for hint, svc := range cases {
		t.Run(hint, func(t *testing.T) {
			if got, want := serviceState(t.Context(), svc, status), "running pid=7 [ready]"; got != want {
				t.Fatalf("serviceState() = %q, want %q", got, want)
			}
		})
	}
}

func serviceWithProbe(t *testing.T) *alphasfile.Service {
	t.Helper()
	return &alphasfile.Service{
		Toolchain: "go",
		Runtime: &alphasfile.RuntimeConfig{
			Name:      "app",
			Readiness: &probe.Probe{TCP: &probe.TCPAction{Port: closedPort(t)}},
		},
	}
}

// closedPort returns a port that was bound and immediately released, so a
// dial against it fails fast instead of hanging on a firewall drop.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}
