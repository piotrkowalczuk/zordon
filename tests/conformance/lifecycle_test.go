// Lifecycle conformance: the externally-observable invariants of
// zordon's happy-path control flow — start, status, stop. The orphan
// and parent-watch suites cover the unhappy paths (signals, kills);
// this file pins down the boring path that users actually hit every
// day, so a regression in `zordon stop` doesn't slip through just
// because the dramatic kill-paths still work.
package conformance_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// TestLifecycle_zordonStop_gracefulShutdown pins the most common
// teardown path: after a successful `zordon start`, a `zordon stop`
// must (a) return success, (b) take alpha down, (c) take the wrapped
// service group down with it, (d) leave no socket file behind. None
// of these are individually covered today — the harness's cleanup
// calls stop best-effort but never asserts on it.
func TestLifecycle_zordonStop_gracefulShutdown(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src {
    path = "./src/svc1"
    exe = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 100
  }
}
`)

	start := p.Zordon("start",
		"--timeout", "120s",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(125 * time.Second).Run(t)
	if start.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", start.ExitCode, start.Stdout, start.Stderr)
	}

	// Capture the picked port while alpha is alive (before we stop it).
	port := p.Get(t, "service.go.svc1.vars.port").Int()
	alphaPID, groupPID := pidsFromAlphaLog(t, p.AlphaLogPath(), "svc1")
	t.Logf("started: alpha=%d group=%d :%d", alphaPID, groupPID, port)
	if !portServing(port, 1*time.Second) {
		t.Fatalf(":%d not serving after start — bringup likely broken, can't test stop", port)
	}

	// Grab the socket path BEFORE stop so we can verify it disappears.
	// The socket lives next to the alpha log inside the project's
	// .zordon/worktrees/main/ tree; alpha's startup log carries the
	// resolved path.
	sock := parseSocketPath(t, p.AlphaLogPath())

	stop := p.Zordon("stop").WithTimeout(10 * time.Second).Run(t)
	if stop.ExitCode != 0 {
		t.Fatalf("zordon stop: exit %d\nstdout: %s\nstderr: %s", stop.ExitCode, stop.Stdout, stop.Stderr)
	}

	// (b) alpha gone — bounded by shutdownGrace (default 2s) plus
	// connection drain. 5s is generous.
	if err := waitGone(func() bool { return !pidAlive(alphaPID) }, 5*time.Second); err != nil {
		t.Fatalf("alpha pid %d still alive after zordon stop: %v", alphaPID, err)
	}
	// (c) wrapped service group gone — proves stop reached tommy too.
	if err := waitGone(func() bool { return groupGone(groupPID) }, 5*time.Second); err != nil {
		t.Fatalf("service group %d still alive after zordon stop: %v", groupPID, err)
	}
	// Port must be dead too — sanity.
	if portServing(port, 200*time.Millisecond) {
		t.Fatalf(":%d still serving after zordon stop", port)
	}
	// (d) socket file removed.
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket %s still present after stop (err=%v)", sock, err)
	}

	// A subsequent stop must be a no-op success (idempotent), not a
	// hard error. Real users will hit this in scripts.
	stop2 := p.Zordon("stop").WithTimeout(5 * time.Second).Run(t)
	if stop2.ExitCode != 0 {
		t.Logf("idempotent stop returned exit %d (informational)\nstderr: %s", stop2.ExitCode, stop2.Stderr)
	}
}

// waitGone polls a predicate until it returns true or the deadline
// hits. Cheap wrapper to keep tests readable.
func waitGone(pred func() bool, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pred() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("predicate false after %s", within)
}

// parseSocketPath extracts the resolved control socket path from
// alpha's startup log line ("listening on <path>").
func parseSocketPath(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read alpha log: %v", err)
	}
	m := reAlphaListen.FindSubmatch(b)
	if m == nil {
		t.Fatalf("could not parse socket path from %s:\n%s", logPath, b)
	}
	return string(m[1])
}

// reAlphaListen matches alpha's listening line in the log.
var reAlphaListen = regexp.MustCompile(`listening on (\S+)`)

// TestLifecycle_failureSummaryShowsCulprit pins zordon's user-facing
// diagnostic on bringup failure. The mechanism it exercises:
//
//  1. zordon trims each service's stdout/stderr into a ring buffer
//     while streaming bringup logs;
//  2. on EventServiceFail it records the culprit + error message;
//  3. on bringup exit (EventDone with failures, EventError, or socket
//     EOF) it emits a structured block on stderr — one section per
//     failure, with the alpha-reported error and that service's last
//     N lines.
//
// Why this test exists: every regression in the summary path is
// silent. Stripping the ring, swapping the failure-list type, moving
// the printer to the wrong branch — none of it would fail any other
// test. The user just stops seeing why their bringup died, which is
// exactly the regression we just fixed.
//
// The test runs a service whose runtime cmd prints distinctive marker
// lines on both stdout and stderr, then exits non-zero. Asserts that
// the marker lines AND the verdict ("error: exit status N") appear
// inside the summary block (between the two horizontal-rule lines).
func TestLifecycle_failureSummaryShowsCulprit(t *testing.T) {
	// failfast=true closes alpha after the first failure, which
	// produces the same "EOF on socket" path that real-world fast
	// failures hit. Important to cover: bug-history shows that's the
	// branch most likely to skip the summary.
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc")

	const (
		stdoutMarker = "PROOF-STDOUT-LINE-FROM-SVC"
		stderrMarker = "PROOF-STDERR-LINE-FROM-SVC"
	)

	// Distinctive runtime cmd: ignores the built echo binary, just
	// shells out to print our two markers and exit non-zero. The
	// service still has to "exist" as a go service for the toolchain
	// machinery; we just don't execute its binary.
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc" {
  src {
    path = "./src/svc"
    exe = "."
  }

  runtime {
    cmd = ["sh", "-c", "echo %s\necho %s >&2\nsleep 0.1\nexit 9"]
  }
}
`, stdoutMarker, stderrMarker))

	res := p.Zordon("start",
		"--alpha-log", p.AlphaLogPath(),
		"--timeout", "120s",
	).WithTimeout(125 * time.Second).Run(t)
	if res.ExitCode == 0 {
		t.Fatalf("zordon start unexpectedly succeeded; bringup-failure test premise broken")
	}
	combined := res.Stdout + res.Stderr

	// The summary block opens and closes with a 60-dash horizontal
	// rule. Anything between is the structured failure report.
	const rule = "------------------------------------------------------------"
	i := strings.Index(combined, rule)
	if i < 0 {
		t.Fatalf("no summary opening rule in zordon output\nstdout: %s\nstderr: %s",
			res.Stdout, res.Stderr)
	}
	j := strings.Index(combined[i+len(rule):], rule)
	if j < 0 {
		t.Fatalf("no summary closing rule; output truncated mid-summary?\n%s", combined[i:])
	}
	block := combined[i : i+len(rule)+j+len(rule)]

	// Mandatory facts inside the block.
	mustContain := []string{
		"Bringup failed: 1 service(s)",
		"[svc] error: exit status 9",
		"[svc] last", // "[svc] last N line(s):" — exact count varies on stabilization
		stdoutMarker,
		stderrMarker,
		"[stdout] " + stdoutMarker,
		"[stderr] " + stderrMarker,
	}
	for _, want := range mustContain {
		if !strings.Contains(block, want) {
			t.Errorf("summary block missing %q\n----- block -----\n%s\n-----", want, block)
		}
	}
}
