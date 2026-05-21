// Parent-watch conformance: when zordon is killed mid-bringup, alpha
// must observe the death and shut itself + its services down — even
// though zordon ran zero cleanup code. The mechanism is the inherited
// pipe in internal/parentwatch: zordon holds the write end open across
// the entire bringup (until pushConfigure returns on EventDone), so a
// SIGKILL'd zordon causes the kernel to close that write end, which
// EOFs alpha's read end and trips a graceful shutdown.
//
// The test widens the bringup window by giving the echo service a
// readiness delay long enough to land a deterministic SIGKILL inside
// it. Without --ready-delay the bringup is too fast to reliably hit.
package conformance_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestParentWatch_zordonKilledDuringBringup_alphaShutsDown(t *testing.T) {
	// zordon dies via SIGKILL and never runs its cleanup, so the
	// in-process socket/registry cleanup that the harness leans on
	// won't run either. Tell the harness not to fail on leftovers;
	// we sweep manually below.
	p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
	p.CopyTree("golden/go/echo", "src/svc1")

	// readyDelay is the artificial bringup widener. alpha probes /
	// every 200ms and the service refuses to bind for readyDelay, so
	// the window in which "zordon is alive AND bringup is incomplete"
	// is at least readyDelay long — comfortably more than the 1-2s we
	// need to land a kill.
	const readyDelay = 5 * time.Second
	const port = 27951

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
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}", "-ready-delay", "%s"]
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
`, port, readyDelay))

	// Spawn zordon directly so we can SIGKILL it. The harness Run()
	// blocks until exit — useless when we need to kill mid-flight.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Same binary resolution as the rest of the conformance suite:
	// $ZORDON_BIN > `zordon` on $PATH. The suite assumes the dev has
	// already installed a fresh build (`go install ./cmd/...`) before
	// running tests; we don't second-guess that here. alpha is resolved
	// by zordon itself from $PATH ("alpha") and tommy is a sibling of
	// alpha — also covered by the same `go install`.
	binZ := zordonBinaryFromEnvOrPath(t)
	cmd := exec.CommandContext(ctx, binZ,
		"start",
		"--timeout", "90s",
		"--alpha-log", p.AlphaLogPath(),
	)
	cmd.Dir = p.Dir()
	// Mirror what zordontest.Project.env() injects: keep host env so
	// git/mise resolve, and stamp the harness markers + ZORDON_HOME.
	cmd.Env = append(os.Environ(),
		"ZORDON_HOME="+p.Home(),
		"ZORDON_TEST_HARNESS=1",
	)
	// Capture so we can dump on failure. Stdout/stderr stay local.
	stdout, err := os.CreateTemp(t.TempDir(), "zordon.stdout.*")
	if err != nil {
		t.Fatalf("tmp stdout: %v", err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "zordon.stderr.*")
	if err != nil {
		t.Fatalf("tmp stderr: %v", err)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start zordon: %v", err)
	}
	zordonPID := cmd.Process.Pid
	t.Logf("zordon started pid=%d", zordonPID)

	// Always reap zordon on test end — if we somehow fail before the
	// SIGKILL, don't leak the process into the host.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		dumpAlphaLog(t, p.AlphaLogPath())
		if s, err := os.ReadFile(stdout.Name()); err == nil && len(s) > 0 {
			t.Logf("zordon stdout:\n%s", s)
		}
		if s, err := os.ReadFile(stderr.Name()); err == nil && len(s) > 0 {
			t.Logf("zordon stderr:\n%s", s)
		}
	})

	// Wait until alpha has started the service. That's the moment we
	// know bringup is in flight — readiness probing started, but the
	// service is still sleeping inside ready-delay.
	alphaPID := waitForAlphaServiceStart(t, p.AlphaLogPath(), "svc1", 60*time.Second)
	t.Logf("alpha pid=%d, svc1 spawned with %s ready-delay — killing zordon", alphaPID, readyDelay)

	// SIGKILL zordon. Mirrors OOM-kill: no defers run, no socket
	// close, no shutdown message. The parent-watch pipe is alpha's
	// only signal that anything happened.
	if err := syscall.Kill(zordonPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill zordon pid %d: %v", zordonPID, err)
	}

	// alpha should observe the pipe EOF within milliseconds and run
	// graceful shutdown (which itself bounded by --shutdown-grace,
	// default 2s). Give it a generous 10s before declaring failure.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(alphaPID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(alphaPID) {
		// Best-effort cleanup so the host isn't polluted, then fail.
		_ = syscall.Kill(alphaPID, syscall.SIGKILL)
		t.Fatalf("alpha pid %d still alive 10s after zordon SIGKILL — parent-watch did not trigger shutdown", alphaPID)
	}

	// Belt-and-braces: the service port must be dead too. If alpha
	// died but the wrapped service group was orphaned, that would
	// indicate a regression in the alpha→tommy parent-watch wiring.
	if portServing(port, 200*time.Millisecond) {
		t.Fatalf("alpha exited but :%d still serving — service was orphaned", port)
	}

	// Confirm the shutdown reason came from parent-watch, not some
	// other signal we accidentally delivered.
	logBytes, err := os.ReadFile(p.AlphaLogPath())
	if err != nil {
		t.Fatalf("read alpha log: %v", err)
	}
	if !strings.Contains(string(logBytes), "zordon parent died") {
		t.Fatalf("alpha log does not mention parent-death trigger; got:\n%s", logBytes)
	}
}

// TestParentWatch_zordonCleanExit_alphaSurvives is the dual of the
// kill-during-bringup test: when zordon completes normally (EventDone,
// then exit), alpha must NOT interpret the kernel-closed write end of
// its parent-watch pipe as death. Otherwise every successful
// `zordon start` would silently take down the alpha it just brought
// up.
//
// Regression target: `defer state.parentW.Stop()` in handleConfigure.
// Removing or misplacing that line causes alpha to shutdown ~instantly
// after zordon's clean exit, and nothing else catches it.
func TestParentWatch_zordonCleanExit_alphaSurvives(t *testing.T) {
	// We expect alpha to keep running past `zordon start`, so the
	// harness's cleanup-time `zordon stop` is the right tear-down.
	// No --ready-delay: bringup finishes ASAP so we minimize the
	// flaky-by-slow window.
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")

	const port = 27952
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

	res := p.Zordon("start",
		"--timeout", "120s",
		"--alpha-log", p.AlphaLogPath(),
	).
		WithTimeout(125 * time.Second).
		Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	alphaPID, _ := pidsFromAlphaLog(t, p.AlphaLogPath(), "svc1")
	t.Logf("zordon detached; alpha pid=%d should keep running", alphaPID)

	// Settle window: if Stop() wasn't called, parent-watch's Died()
	// fires within ~1ms of zordon exit, alpha calls requestShutdown,
	// and shutdownAll takes shutdownGrace (~2s) to wind everything
	// down. A 3s observation more than covers both bounds.
	settle := time.NewTimer(3 * time.Second)
	defer settle.Stop()
	probe := time.NewTicker(200 * time.Millisecond)
	defer probe.Stop()
loop:
	for {
		select {
		case <-settle.C:
			break loop
		case <-probe.C:
			if !pidAlive(alphaPID) {
				t.Fatalf("alpha pid %d died after zordon clean exit — parent-watch did not Stop", alphaPID)
			}
			if !portServing(port, 200*time.Millisecond) {
				t.Fatalf("alpha alive but :%d stopped serving — service supervised down somehow", port)
			}
		}
	}

	// Final positive check via the control socket: `zordon status` is
	// the user-visible "alpha is healthy" probe.
	st := p.Zordon("status").WithTimeout(5 * time.Second).Run(t)
	if st.ExitCode != 0 {
		t.Fatalf("zordon status after clean detach: exit %d\nstdout: %s\nstderr: %s",
			st.ExitCode, st.Stdout, st.Stderr)
	}

	// And make sure alpha never logged the parent-death trigger.
	logBytes, err := os.ReadFile(p.AlphaLogPath())
	if err != nil {
		t.Fatalf("read alpha log: %v", err)
	}
	if strings.Contains(string(logBytes), "zordon parent died") {
		t.Fatalf("alpha logged parent-death trigger on a CLEAN zordon exit — Stop() race lost or missing")
	}
}

// zordonBinaryFromEnvOrPath mirrors zordontest.resolveZordonBinary's
// resolution order (which is unexported, so we replicate it here):
// $ZORDON_BIN absolute path first, then `zordon` on $PATH. Fails the
// test if neither resolves.
func zordonBinaryFromEnvOrPath(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("ZORDON_BIN"); v != "" {
		return v
	}
	p, err := exec.LookPath("zordon")
	if err != nil {
		t.Fatalf("zordon not on $PATH and $ZORDON_BIN unset; run `go install ./cmd/...` first")
	}
	return p
}

// waitForAlphaServiceStart reads alpha.log until it sees both the
// "starting pid=" line and a "started <svc>" line, then returns
// alpha's own pid. Bounded by deadline.
func waitForAlphaServiceStart(t *testing.T, logPath, svc string, within time.Duration) int {
	t.Helper()
	want := "started " + svc
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(logPath)
		if err == nil {
			s := string(b)
			if strings.Contains(s, want) {
				if m := reAlphaStart.FindStringSubmatch(s); m != nil {
					var pid int
					if _, e := fmt.Sscanf(m[1], "%d", &pid); e == nil {
						return pid
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("alpha did not start %q within %s; log path %s", svc, within, logPath)
	return 0
}

// pidAlive reports whether the given pid has not been reaped yet.
// kill(pid, 0) returns ESRCH for a dead/reaped process.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
