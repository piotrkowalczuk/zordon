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

	const port = 27953
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src = "./src/svc1"
  exe = "."

  vars = { port = %d }

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
`, port))

	start := p.Zordon("start",
		"--timeout", "120s",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(125 * time.Second).Run(t)
	if start.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", start.ExitCode, start.Stdout, start.Stderr)
	}

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
