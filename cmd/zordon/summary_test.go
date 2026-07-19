package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/summary"
	"github.com/piotrkowalczuk/zordon/internal/zlog"
)

func TestPrintStartSummary(t *testing.T) {
	var buf bytes.Buffer
	log := zlog.New(&buf, false) // non-terminal writer → no color, plain text

	s := &summary.StartSummary{
		TotalMS: 4210,
		Services: []summary.ServiceTiming{
			{Name: "db", Toolchain: "go", BuildMS: 1400, SpawnMS: 10, ReadyMS: 390, TotalMS: 1800},
			{Name: "api", Toolchain: "go", After: []string{"service.go.db@ready"}, WaitMS: 1810, BuildMS: 200, SpawnMS: 20, ReadyMS: 380, TotalMS: 2410},
		},
		Provisions: []summary.ProvisionTiming{
			{Name: "seed", Service: "api", After: []string{"service.go.db@ready"}, RunMS: 1100},
			{Name: "warmup", Service: "api", Detached: true},
		},
	}

	printStartSummary(log, s)
	out := buf.String()

	for _, want := range []string{
		"Bringup complete: 2 service(s), 2 provision(s) in 4.21s",
		"wait", "build", "spawn", "ready", "total", // phase column headers
		"db", "api",
		"1.40s", // db build duration
		"after: service.go.db@ready",
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
