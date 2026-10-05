package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
)

// Output formats of --format. Left unset it is FormatAgent under --agent and
// FormatText otherwise.
const (
	FormatText  = "text"
	FormatAgent = "agent"
	FormatJSON  = "json"
)

// Stack states of a StatusReport.
const (
	StackRunning      = "running"
	StackStopped      = "stopped"
	StackError        = "error"
	StackNoAlphasfile = "no-alphasfile"
)

// Service states of a StatusReport.
const (
	ServiceReady     = "ready"
	ServiceUnhealthy = "unhealthy"
	ServiceProbing   = "probing"
	ServiceStarting  = "starting"
	ServiceFailed    = "failed"
	ServiceStopped   = "stopped"
)

// StatusReport is the invocation's stack as `zordon status` reports it in the
// agent and json formats: only what an agent acts on. Fields at their zero
// value are left out.
type StatusReport struct {
	Workspace  string          `json:"workspace,omitempty"`
	Alphasfile string          `json:"alphasfile,omitempty"`
	StateDir   string          `json:"state_dir,omitempty"`
	State      string          `json:"state"`
	Error      string          `json:"error,omitempty"`
	Services   []StatusService `json:"services,omitempty"`
}

// StatusService is one service of a StatusReport.
type StatusService struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Health   string `json:"health,omitempty"`   // why an unhealthy service failed its probe
	Picked   bool   `json:"picked,omitempty"`   // the workspace's own worktree
	Shared   bool   `json:"shared,omitempty"`   // run by a federation parent, not this invocation
	Print    string `json:"print,omitempty"`    // the service's composed print line
	Checkout string `json:"checkout,omitempty"` // set only when off its canonical branch
	Revision string `json:"revision,omitempty"` // a detached checkout's short commit
}

// outputFormat resolves --format against --agent, which defaults it.
func outputFormat(format string, agent bool) (string, error) {
	switch format {
	case "":
		if agent {
			return FormatAgent, nil
		}
		return FormatText, nil
	case FormatText, FormatAgent, FormatJSON:
		return format, nil
	}
	return "", fmt.Errorf("--format %q: want %s, %s or %s", format, FormatText, FormatAgent, FormatJSON)
}

// runStatusReport writes the stack in the agent or json format. It always
// writes a report and returns nil: an error resolving the stack is a state of
// the report, so a caller parses stdout whatever happened.
func runStatusReport(ctx context.Context, out io.Writer, zordonHome string, testCfg alphasfile.TestConfig, format string) error {
	r := statusReport(ctx, zordonHome, testCfg)
	switch format {
	case FormatJSON:
		return json.NewEncoder(out).Encode(r)
	case FormatAgent:
		_, err := io.WriteString(out, r.logfmt())
		return err
	}
	panic(fmt.Sprintf("runStatusReport: format %q has no report", format))
}

// logfmt renders the report as logfmt lines: the stack, then one per service.
func (r StatusReport) logfmt() string {
	var b strings.Builder
	writeLogfmt(&b, "workspace", r.Workspace, "state", r.State, "alphasfile", r.Alphasfile, "state_dir", r.StateDir, "error", r.Error)
	for _, s := range r.Services {
		writeLogfmt(&b,
			"service", s.Name,
			"state", s.State,
			"health", s.Health,
			"picked", boolField(s.Picked),
			"shared", boolField(s.Shared),
			"print", s.Print,
			"checkout", s.Checkout,
			"revision", s.Revision,
		)
	}
	return b.String()
}

func statusReport(ctx context.Context, zordonHome string, testCfg alphasfile.TestConfig) StatusReport {
	levels, err := resolveChain(ctx, zordonHome, testCfg)
	switch {
	case errors.Is(err, invocation.ErrNoAlphasfile):
		return StatusReport{State: StackNoAlphasfile}
	case err != nil:
		return StatusReport{State: StackError, Error: err.Error()}
	}

	var r StatusReport
	for _, lv := range levels {
		if lv.isInvocation {
			r.Workspace = lv.inv.Workspace
			r.Alphasfile = lv.afPath
			r.StateDir = lv.inv.StateDir
			r.State = StackStopped
			if live(lv.state) {
				r.State = StackRunning
			}
		}
		if lv.state == nil {
			continue
		}
		r.Services = append(r.Services, reportServices(ctx, lv)...)
	}
	return r
}

// reportServices lists a level's services by the name status and zordon start
// both accept. A level whose alpha does not run reports no print line, as
// its ports were picked for the report only.
func reportServices(ctx context.Context, lv *level) []StatusService {
	running := make(map[string]protocol.ServiceStatus, len(lv.state.Running))
	for _, r := range lv.state.Running {
		running[r.Name] = r
	}
	out := make([]StatusService, 0, len(lv.state.Services))
	for _, s := range lv.state.Services {
		svc := StatusService{
			Name:   s.ShortName(),
			State:  ServiceStopped,
			Shared: !lv.isInvocation,
			Picked: lv.isInvocation && lv.inv.OwnsService(s.Name()),
		}
		if status, ok := running[s.Name()]; ok {
			svc.State, svc.Health = reportServiceState(ctx, s, status)
		}
		if live(lv.state) && s.Runtime != nil {
			svc.Print = s.Runtime.Print
		}
		if co, ok := checkoutOf(ctx, s, lv.inv.Workspace); ok {
			svc.Revision = co.SHA
			if co.isDrifted() {
				svc.Checkout = fmt.Sprintf("on branch %s, not %s", co.Ref, co.Want)
			}
		}
		out = append(out, svc)
	}
	return out
}

// reportServiceState is serviceState's verdict as a state name, plus the
// probe error of a ready service that no longer answers.
func reportServiceState(ctx context.Context, s *alphasfile.Service, status protocol.ServiceStatus) (state, health string) {
	switch status.Readiness {
	case protocol.ReadinessReady:
		if err := healthErr(ctx, s); err != nil {
			return ServiceUnhealthy, err.Error()
		}
		return ServiceReady, ""
	case protocol.ReadinessProbing:
		return ServiceProbing, ""
	case protocol.ReadinessFailed:
		return ServiceFailed, ""
	}
	return ServiceStarting, ""
}

// writeLogfmt writes one line of key=value pairs, skipping empty values and
// quoting a value that holds a space, a quote, an equals sign or a control
// character.
func writeLogfmt(b *strings.Builder, kv ...string) {
	if len(kv)%2 != 0 {
		panic("writeLogfmt: odd number of key/value arguments")
	}
	first := true
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		if !first {
			b.WriteByte(' ')
		}
		first = false
		b.WriteString(kv[i])
		b.WriteByte('=')
		b.WriteString(logfmtValue(kv[i+1]))
	}
	b.WriteByte('\n')
}

func logfmtValue(v string) string {
	if strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == '"' || r == '=' || r == 0x7f }) {
		return strconv.Quote(v)
	}
	return v
}

func boolField(v bool) string {
	if v {
		return "true"
	}
	return ""
}
