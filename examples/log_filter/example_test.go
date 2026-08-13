// Package example_test runs the log_filter example as a conformance test:
// a service emits a mix of lines, and the per-service log { filter } block
// drops the noisy ones at the source. The test asserts that dropped lines
// never reach alpha's log while kept lines do.
package example_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestExample_logFilter drives the real stack. The service prints four
// tagged lines then "up <addr>" on stdout before serving; the filter drops
// the debug + backtrace lines. Because "up" is emitted last on the same
// stream, its arrival in alpha's log proves the whole stdout stream drained,
// making the drop assertions sound (a dropped line can't still be in flight).
//
// Not parallel and not cheap: it builds the pinned Go toolchain and the
// example on a cold cache (~minutes), like the other example tests.
func TestExample_logFilter(t *testing.T) {
	p := zordontest.NewProject(t, zordontest.WithCallerRoot())

	// WithCallerRoot keeps alpha.log in the example dir, where it would
	// otherwise accumulate across runs; start from a clean file so the
	// "dropped lines are absent" assertions see only this run's output.
	if err := zfs.RemoveIfPresent(p.AlphaLogPath()); err != nil {
		t.Fatalf("reset alpha log: %v", err)
	}

	p.Start(t).OK()

	port := p.Get(t, "service.go.app.vars.port").Int()
	if port <= 0 {
		t.Fatalf("port = %d; want a valid TCP port", port)
	}

	waitFor(t, p, fmt.Sprintf("up 127.0.0.1:%d", port))

	log := p.AlphaLog()
	for _, keep := range []string{"KEEPME_ERROR", "KEEPME_PLAIN"} {
		if !log.Contains(keep) {
			t.Errorf("alpha log missing kept line %q", keep)
		}
	}
	for _, drop := range []string{"DROPME_DEBUG", "server.rb:42"} {
		if log.Contains(drop) {
			t.Errorf("alpha log contains dropped line %q (filter did not drop it)", drop)
		}
	}
}

// waitFor polls alpha's log (re-read each call) until substr appears, so the
// assertions run against a settled stream rather than racing the drain.
func waitFor(t *testing.T, p *zordontest.Project, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.AlphaLog().Contains(substr) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("alpha log never contained %q", substr)
}
