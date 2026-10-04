package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

// Stack states of `zordon --agent status`.
const (
	AgentStackRunning      = "running"
	AgentStackStopped      = "stopped"
	AgentStackError        = "error"
	AgentStackNoAlphasfile = "no-alphasfile"
)

// Service states of `zordon --agent status`.
const (
	AgentServiceReady     = "ready"
	AgentServiceUnhealthy = "unhealthy"
	AgentServiceProbing   = "probing"
	AgentServiceStarting  = "starting"
	AgentServiceFailed    = "failed"
	AgentServiceStopped   = "stopped"
)

// AgentStatus is what `zordon --agent status` prints: the invocation's stack
// with only what an agent acts on, as one JSON object. Fields at their zero
// value are left out.
type AgentStatus struct {
	Workspace  string         `json:"workspace,omitempty"`
	Alphasfile string         `json:"alphasfile,omitempty"`
	StateDir   string         `json:"state_dir,omitempty"`
	State      string         `json:"state"`
	Error      string         `json:"error,omitempty"`
	Services   []AgentService `json:"services,omitempty"`
}

// AgentService is one service of AgentStatus.
type AgentService struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Health   string `json:"health,omitempty"`   // why an unhealthy service failed its probe
	Picked   bool   `json:"picked,omitempty"`   // the workspace's own worktree
	Shared   bool   `json:"shared,omitempty"`   // run by a federation parent, not this invocation
	Print    string `json:"print,omitempty"`    // the service's composed print line
	Checkout string `json:"checkout,omitempty"` // set only when off its canonical branch
}

// runStatusAgent reports the stack as AgentStatus. It always writes one
// object and returns nil: an error resolving the stack is a state of the
// report, so a caller parses stdout whatever happened.
func runStatusAgent(ctx context.Context, out io.Writer, zordonHome string, testCfg alphasfile.TestConfig) error {
	return json.NewEncoder(out).Encode(agentStatus(ctx, zordonHome, testCfg))
}

func agentStatus(ctx context.Context, zordonHome string, testCfg alphasfile.TestConfig) AgentStatus {
	levels, err := resolveChain(ctx, zordonHome, testCfg)
	switch {
	case errors.Is(err, invocation.ErrNoAlphasfile):
		return AgentStatus{State: AgentStackNoAlphasfile}
	case err != nil:
		return AgentStatus{State: AgentStackError, Error: err.Error()}
	}

	var st AgentStatus
	for _, lv := range levels {
		if lv.isInvocation {
			st.Workspace = lv.inv.Workspace
			st.Alphasfile = lv.afPath
			st.StateDir = lv.inv.StateDir
			st.State = AgentStackStopped
			if isAlphaRunning(lv.state) {
				st.State = AgentStackRunning
			}
		}
		if lv.state == nil {
			continue
		}
		st.Services = append(st.Services, agentServices(ctx, lv)...)
	}
	return st
}

// isAlphaRunning tells a live alpha's state from the static one resolveChain
// evaluates from disk when none answers, which carries no PID.
func isAlphaRunning(st *protocol.StateInfo) bool {
	return st != nil && st.PID > 0
}

func agentServices(ctx context.Context, lv *level) []AgentService {
	running := make(map[string]protocol.ServiceStatus, len(lv.state.Running))
	for _, r := range lv.state.Running {
		running[r.Name] = r
	}
	out := make([]AgentService, 0, len(lv.state.Services))
	for _, s := range lv.state.Services {
		svc := AgentService{
			Name:   s.Name(),
			State:  AgentServiceStopped,
			Shared: !lv.isInvocation,
			Picked: lv.isInvocation && lv.inv.OwnsService(s.Name()),
		}
		if status, ok := running[s.Name()]; ok {
			svc.State, svc.Health = agentServiceState(ctx, s, status)
		}
		if s.Runtime != nil {
			svc.Print = s.Runtime.Print
		}
		if co, ok := checkoutOf(ctx, s, lv.inv.Workspace); ok && co.isDrifted() {
			svc.Checkout = fmt.Sprintf("on branch %s, not %s", co.Ref, co.Want)
		}
		out = append(out, svc)
	}
	return out
}

// agentServiceState is serviceState's verdict as a state name, plus the probe
// error of a ready service that no longer answers.
func agentServiceState(ctx context.Context, s *alphasfile.Service, status protocol.ServiceStatus) (state, health string) {
	switch status.Readiness {
	case protocol.ReadinessReady:
		if err := healthErr(ctx, s); err != nil {
			return AgentServiceUnhealthy, err.Error()
		}
		return AgentServiceReady, ""
	case protocol.ReadinessProbing:
		return AgentServiceProbing, ""
	case protocol.ReadinessFailed:
		return AgentServiceFailed, ""
	}
	return AgentServiceStarting, ""
}
