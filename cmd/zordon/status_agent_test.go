package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

func TestAgentServiceState(t *testing.T) {
	cases := map[string]struct {
		status protocol.ServiceStatus
		want   string
	}{
		"not spawned yet":   {protocol.ServiceStatus{}, AgentServiceStarting},
		"probing":           {protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessProbing}, AgentServiceProbing},
		"failed":            {protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessFailed}, AgentServiceFailed},
		"unknown readiness": {protocol.ServiceStatus{PID: 7, Readiness: "wat"}, AgentServiceStarting},
	}

	svc := serviceWithProbe(t)
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, health := agentServiceState(t.Context(), svc, c.status)
			if got != c.want || health != "" {
				t.Fatalf("agentServiceState() = (%q, %q), want (%q, \"\")", got, health, c.want)
			}
		})
	}
}

func TestAgentServiceState_unhealthy(t *testing.T) {
	status := protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessReady}

	got, health := agentServiceState(t.Context(), serviceWithProbe(t), status)

	if got != AgentServiceUnhealthy || health == "" {
		t.Fatalf("agentServiceState() = (%q, %q), want (%q, a probe error)", got, health, AgentServiceUnhealthy)
	}
}

func TestAgentServices_leaf(t *testing.T) {
	lv := &level{
		isInvocation: true,
		inv:          &invocation.InvocationState{Workspace: "feature", OwnedServices: map[string]bool{"api": true}},
		state: &protocol.StateInfo{
			Services: []*alphasfile.Service{
				agentTestService("api", "http://localhost:8080"),
				agentTestService("worker", ""),
			},
			Running: []protocol.ServiceStatus{{Name: "api", PID: 7, Readiness: protocol.ReadinessReady}},
		},
	}

	got := agentServices(t.Context(), lv)

	want := []AgentService{
		{Name: "api", State: AgentServiceReady, Picked: true, Print: "http://localhost:8080"},
		{Name: "worker", State: AgentServiceStopped},
	}
	assertAgentServices(t, got, want)
}

func TestAgentServices_federationParent(t *testing.T) {
	lv := &level{
		inv: &invocation.InvocationState{Workspace: invocation.MainWorkspace},
		state: &protocol.StateInfo{
			Services: []*alphasfile.Service{agentTestService("postgres", "")},
			Running:  []protocol.ServiceStatus{{Name: "postgres", PID: 9, Readiness: protocol.ReadinessProbing}},
		},
	}

	got := agentServices(t.Context(), lv)

	assertAgentServices(t, got, []AgentService{{Name: "postgres", State: AgentServiceProbing, Shared: true}})
}

func TestIsAlphaRunning(t *testing.T) {
	cases := map[string]struct {
		state *protocol.StateInfo
		want  bool
	}{
		"no state":                {nil, false},
		"static, from Alphasfile": {&protocol.StateInfo{Services: []*alphasfile.Service{agentTestService("api", "")}}, false},
		"live alpha":              {&protocol.StateInfo{PID: 42}, true},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := isAlphaRunning(c.state); got != c.want {
				t.Fatalf("isAlphaRunning() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRunStatusAgent_noAlphasfile(t *testing.T) {
	t.Chdir(t.TempDir())
	var out bytes.Buffer

	if err := runStatusAgent(t.Context(), &out, t.TempDir(), alphasfile.TestConfig{}); err != nil {
		t.Fatalf("runStatusAgent() error = %v, want nil", err)
	}

	if got, want := out.String(), `{"state":"no-alphasfile"}`+"\n"; got != want {
		t.Fatalf("runStatusAgent() wrote %q, want %q", got, want)
	}
}

func agentTestService(name, print string) *alphasfile.Service {
	return &alphasfile.Service{Toolchain: "go", Runtime: &alphasfile.RuntimeConfig{Name: name, Print: print}}
}

func assertAgentServices(t *testing.T, got, want []AgentService) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("agentServices() = %s, want %s", g, w)
	}
}
