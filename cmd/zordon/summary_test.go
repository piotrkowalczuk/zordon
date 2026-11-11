package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/ring"
	"github.com/piotrkowalczuk/zordon/internal/summary"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestPrintStartSummary(t *testing.T) {
	var buf bytes.Buffer

	s := &summary.StartSummary{
		TotalMS: 4210,
		Services: []summary.ServiceTiming{
			{Name: "db", Toolchain: "go", BuildMS: 1400, SpawnMS: 10, ReadyMS: 390, TotalMS: 1800,
				Deps: []summary.DepTiming{{Ref: "toolchain.go@ready", WaitMS: 0}}},
			{Name: "api", Toolchain: "go", WaitMS: 1810, BuildMS: 200, SpawnMS: 20, ReadyMS: 380, TotalMS: 2410,
				Deps: []summary.DepTiming{
					{Ref: "toolchain.go@ready", WaitMS: 0},
					{Ref: "service.go.db.runtime@ready", WaitMS: 1810, LongPole: true},
				}},
		},
		Provisions: []summary.ProvisionTiming{
			{Name: "seed", Service: "api", After: []string{"service.go.db@ready"}, RunMS: 1100},
			{Name: "warmup", Service: "api", Detached: true},
		},
	}

	printStartSummary(&buf, s)
	out := buf.String()

	for _, want := range []string{
		"Bringup complete: 2 service(s), 2 provision(s) in 4.21s",
		"wait", "build", "spawn", "ready", "total", // phase column headers
		"db", "api",
		"1.40s", // db build duration
		"after:",
		"toolchain.go@ready",          // implicit toolchain dep is surfaced
		"service.go.db.runtime@ready", // api's declared dep
		"<- long pole",                // the dep that gated api's start
		"1.81s",                       // api's wait on the long-pole dep
		"provisions:",
		"seed", "1.10s",
		"warmup", "detached (running)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary output missing %q\n----- output -----\n%s", want, out)
		}
	}

	// Delimited by two 60-dash rules, like the failure summary — that's the
	// block the conformance test and users scan for.
	const rule = "------------------------------------------------------------"
	if got := strings.Count(out, rule); got != 2 {
		t.Errorf("want exactly two %d-dash rules, got %d\n%s", len(rule), got, out)
	}
}

func TestPrintFailureSummary(t *testing.T) {
	tail := ring.New(10)
	tail.Push("[stderr] downloading mise v2026.11.6...")
	tail.Push("[stderr] sha256 mismatch")

	var withLogBuf bytes.Buffer
	printFailureSummary(
		zlog.New(&withLogBuf, true),
		[]failure{
			{service: "toolchain/go", err: "ensure mise: checksum failed"},
			{service: "api", err: "exited before ready"},
		},
		map[string]*ring.Buffer{"toolchain/go": tail},
		"/tmp/project/.zordon/alpha.log",
	)
	gotWithLog := withLogBuf.String()
	for _, want := range []string{
		"Bringup failed: 2 service(s)",
		"[toolchain/go] error: ensure mise: checksum failed",
		"[toolchain/go] last 2 line(s):",
		"[stderr] sha256 mismatch",
		"[api] (no service output captured; see alpha log /tmp/project/.zordon/alpha.log)",
	} {
		if !strings.Contains(gotWithLog, want) {
			t.Errorf("summary missing %q:\n%s", want, gotWithLog)
		}
	}

	var withoutLogBuf bytes.Buffer
	printFailureSummary(
		zlog.New(&withoutLogBuf, true),
		[]failure{{service: "api", err: "exited before ready"}},
		nil,
		"",
	)
	if !strings.Contains(withoutLogBuf.String(), "[api] (no service output captured)") {
		t.Errorf("expected fallback message when alphaLog is empty, got:\n%s", withoutLogBuf.String())
	}
}
