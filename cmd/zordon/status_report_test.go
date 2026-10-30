package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/alphasfile"
	"github.com/piotrkowalczuk/zordon/internal/invocation"
	"github.com/piotrkowalczuk/zordon/internal/protocol"
	"github.com/piotrkowalczuk/zordon/internal/zlock"
	"github.com/piotrkowalczuk/zordon/internal/ztest"
)

func TestOutputFormat(t *testing.T) {
	cases := map[string]struct {
		format string
		agent  bool
		want   string
	}{
		"default":                  {"", false, FormatText},
		"--agent defaults agent":   {"", true, FormatAgent},
		"--format wins on --agent": {FormatText, true, FormatText},
		"json":                     {FormatJSON, false, FormatJSON},
		"json under --agent":       {FormatJSON, true, FormatJSON},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, err := outputFormat(c.format, c.agent)
			if err != nil || got != c.want {
				t.Fatalf("outputFormat(%q, %v) = (%q, %v), want (%q, nil)", c.format, c.agent, got, err, c.want)
			}
		})
	}
}

func TestOutputFormat_unknown(t *testing.T) {
	if _, err := outputFormat("yaml", false); err == nil {
		t.Fatal("outputFormat(\"yaml\") error = nil, want one naming the formats")
	}
}

func TestReportServiceState(t *testing.T) {
	cases := map[string]struct {
		status protocol.ServiceStatus
		want   string
	}{
		"not spawned yet":   {protocol.ServiceStatus{}, ServiceStarting},
		"probing":           {protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessProbing}, ServiceProbing},
		"failed":            {protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessFailed}, ServiceFailed},
		"unknown readiness": {protocol.ServiceStatus{PID: 7, Readiness: "wat"}, ServiceStarting},
	}

	svc := serviceWithProbe(t)
	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got, health := reportServiceState(t.Context(), svc, c.status)
			if got != c.want || health != "" {
				t.Fatalf("reportServiceState() = (%q, %q), want (%q, \"\")", got, health, c.want)
			}
		})
	}
}

func TestReportServiceState_unhealthy(t *testing.T) {
	status := protocol.ServiceStatus{PID: 7, Readiness: protocol.ReadinessReady}

	got, health := reportServiceState(t.Context(), serviceWithProbe(t), status)

	if got != ServiceUnhealthy || health == "" {
		t.Fatalf("reportServiceState() = (%q, %q), want (%q, a probe error)", got, health, ServiceUnhealthy)
	}
}

func TestReportServices_leaf(t *testing.T) {
	lv := &level{
		isInvocation: true,
		inv:          &invocation.InvocationState{Workspace: "feature", OwnedServices: map[string]bool{"api": true}},
		state: &protocol.StateInfo{
			PID: 42,
			Services: []*alphasfile.Service{
				reportTestService("api", "http://localhost:8080"),
				reportTestService("worker", ""),
			},
			Running: []protocol.ServiceStatus{{Name: "api", PID: 7, Readiness: protocol.ReadinessReady}},
		},
	}

	got := reportServices(t.Context(), lv)

	want := []StatusService{
		{Name: "api", State: ServiceReady, Picked: true, Print: "http://localhost:8080"},
		{Name: "worker", State: ServiceStopped},
	}
	assertStatusServices(t, got, want)
}

// A stopped level's ports were picked for the report alone, so its print line
// would name an address nothing listens on.
func TestReportServices_notLive(t *testing.T) {
	lv := &level{
		isInvocation: true,
		inv:          &invocation.InvocationState{Workspace: invocation.MainWorkspace},
		state:        &protocol.StateInfo{Services: []*alphasfile.Service{reportTestService("api", "http://localhost:8080")}},
	}

	got := reportServices(t.Context(), lv)

	assertStatusServices(t, got, []StatusService{{Name: "api", State: ServiceStopped}})
}

func TestReportServices_federationParent(t *testing.T) {
	lv := &level{
		inv: &invocation.InvocationState{Workspace: invocation.MainWorkspace},
		state: &protocol.StateInfo{
			PID:      9,
			Services: []*alphasfile.Service{reportTestService("postgres", "")},
			Running:  []protocol.ServiceStatus{{Name: "postgres", PID: 9, Readiness: protocol.ReadinessProbing}},
		},
	}

	got := reportServices(t.Context(), lv)

	assertStatusServices(t, got, []StatusService{{Name: "postgres", State: ServiceProbing, Shared: true}})
}

func TestReportServices_checkout(t *testing.T) {
	ztest.AssertSystem(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "feature")
	gitIn(t, repo, "-c", "user.email=a@b", "-c", "user.name=z", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
	svc := reportTestService("shop/api", "")
	svc.Runtime.Checkout = repo
	svc.Runtime.Dir = repo + "/shop/api"
	svc.Package = &alphasfile.Package{Toolchain: "go"}
	lv := &level{
		isInvocation: true,
		inv:          &invocation.InvocationState{Workspace: invocation.MainWorkspace},
		state:        &protocol.StateInfo{PID: 42, Services: []*alphasfile.Service{svc}},
	}

	got := reportServices(t.Context(), lv)

	assertStatusServices(t, got, []StatusService{{Name: "shop/api", State: ServiceStopped, CheckoutPath: repo, Branch: "feature", SourceDir: repo + "/shop/api"}})
}

func TestStatusReport_logfmt(t *testing.T) {
	r := StatusReport{
		Workspace:  "feature",
		Alphasfile: "/proj/Alphasfile",
		State:      StackRunning,
		Services: []StatusService{
			{Name: "shop/worker", State: ServiceReady, Picked: true, Print: "http://127.0.0.1:8080/  (shop)"},
			{Name: "db", State: ServiceUnhealthy, Shared: true, Health: `dial "x": refused`, Revision: "0123456789ab"},
			{Name: "web", State: ServiceStopped, CheckoutPath: "/proj/workspaces/feature/src/web", Branch: "zordon/feature/web"},
			{Name: "ui", State: ServiceReady, Behind: 2, BehindRef: "HEAD"},
		},
		Pins: []upstreamPin{{Kind: pinSource, Repo: "github.com/acme/ui", Ref: "HEAD", Commit: "0123456789abcdef", Behind: 2, Fetched: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}},
	}

	want := `workspace=feature state=running alphasfile=/proj/Alphasfile
service=shop/worker state=ready picked=true print="http://127.0.0.1:8080/  (shop)"
service=db state=unhealthy health="dial \"x\": refused" shared=true revision=0123456789ab
service=web state=stopped checkout_path=/proj/workspaces/feature/src/web branch=zordon/feature/web
service=ui state=ready behind=2 behind_ref=HEAD
pin=github.com/acme/ui kind=source ref=HEAD commit=0123456789abcdef behind=2 fetched=2026-10-30T12:00:00Z
`
	if got := r.logfmt(); got != want {
		t.Fatalf("logfmt() =\n%s\nwant\n%s", got, want)
	}
}

func TestStatusReport_json_pins(t *testing.T) {
	r := StatusReport{State: StackStopped, Pins: []upstreamPin{{Kind: pinRepo, Repo: "github.com/acme/infra", Ref: "main", Commit: "0123456789abcdef", Behind: 1, Fetched: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}}}

	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"stopped","pins":[{"kind":"repo","repo":"github.com/acme/infra","ref":"main","commit":"0123456789abcdef","behind":1,"fetched":"2026-10-30T12:00:00Z"}]}`
	if string(got) != want {
		t.Fatalf("json = %s\nwant %s", got, want)
	}
}

func TestRunStatusReport_noAlphasfile(t *testing.T) {
	cases := map[string]struct {
		format string
		want   string
	}{
		"json":  {FormatJSON, `{"state":"no-alphasfile"}` + "\n"},
		"agent": {FormatAgent, "state=no-alphasfile\n"},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var out bytes.Buffer

			home := t.TempDir()
			if err := runStatusReport(t.Context(), &out, home, zlock.NewGlobalKeychain(home), alphasfile.TestConfig{}, c.format); err != nil {
				t.Fatalf("runStatusReport() error = %v, want nil", err)
			}

			if got := out.String(); got != c.want {
				t.Fatalf("runStatusReport() wrote %q, want %q", got, c.want)
			}
		})
	}
}

func reportTestService(name, print string) *alphasfile.Service {
	return &alphasfile.Service{Toolchain: "go", Runtime: &alphasfile.RuntimeConfig{Name: name, Print: print}}
}

func assertStatusServices(t *testing.T, got, want []StatusService) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("reportServices() = %s, want %s", g, w)
	}
}
