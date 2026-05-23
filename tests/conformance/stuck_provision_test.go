// End-to-end reproduction for the provision side of the "alpha hangs
// because a subprocess can't be cancelled" family. Mirrors stuck_build_test
// but exercises runProvision → runShell instead of bringupAndSupervise →
// prepareBuild → newPrepareRunner.
//
// Why a second test even though runShell was already shutdown-aware
// before the prepare-ctx fix:
//
//  1. Regression coverage. The user's actual hang was inside a provision
//     (a `set -euo pipefail` bash script doing concurrent Kafka topic
//     inserts). If we ever rewrite runShell or introduce a new sink in
//     the provision path, this test ensures that channel hookup doesn't
//     silently regress to the build-style "wait forever" behavior.
//
//  2. The provision path has its own escalation chain: SIGTERM the
//     subprocess group, grace, SIGKILL — distinct from the build runner's
//     ctx-based one. Both must work; one passing doesn't imply the other.
package conformance_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestStuckProvision_zordonSIGINTReapsAlphaWithinGrace(t *testing.T) {
	// SIGINT bypasses zordon's graceful path; expect leftovers and
	// sweep alpha ourselves at the bottom. Same shape as restart /
	// parentwatch / stuck_build tests.
	p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
	p.CopyTree("golden/go/echo", "src/svc1")

	const port = 27981
	// The provision traps SIGTERM and sleeps — exactly the misbehaving
	// shape we want the supervisor to reap anyway. svc1 itself has a
	// stable trivial build; the readiness probe never gets to run
	// because bringup is gated on provision success first via the
	// implicit ordering (provision runs in parallel with the service's
	// runtime spawn, but the alpha-wide bringup wait doesn't return
	// EventDone until BOTH the service is ready AND non-detached
	// provisions reached success/failure — so SIGINT lands while the
	// provision is still in flight, which is the window under test).
	p.WriteFile("Alphasfile", fmt.Sprintf(`
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

  vars = { port = %d }

  runtime {
    cmd = ["${fs::bin()}/svc1", "-addr", "127.0.0.1:${self.vars.port}"]

    provision "stuck" {
      cmd = "trap '' TERM; echo provision-stuck-running; sleep 30"
    }
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

	binZ := zordonBinaryFromEnvOrPath(t)
	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	ctx, cancelCtx := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelCtx()

	cmd := exec.CommandContext(ctx, binZ,
		"start",
		"--timeout", "60s",
		"--alpha-log", p.AlphaLogPath(),
	)
	cmd.Dir = p.Dir()
	cmd.Env = append(os.Environ(),
		"ZORDON_HOME="+p.Home(),
		"ZORDON_TEST_HARNESS=1",
	)
	stdout, _ := os.CreateTemp(t.TempDir(), "zordon.stdout.*")
	stderr, _ := os.CreateTemp(t.TempDir(), "zordon.stderr.*")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start zordon: %v", err)
	}
	zordonPID := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the provision's first echo so we know runShell has
	// started cmd.Start and is now in c.Wait — i.e. the SIGINT race
	// window is open.
	waitForLogContent(t, p.AlphaLogPath(), "provision-stuck-running", 60*time.Second)
	alphaPID := lastAlphaPIDFromLog(t, p.AlphaLogPath())
	if alphaPID <= 0 {
		t.Fatalf("could not read alpha pid from log")
	}
	t.Logf("alpha pid=%d, provision is sleeping with SIGTERM trapped — SIGINT zordon now", alphaPID)

	if err := syscall.Kill(zordonPID, syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT zordon pid %d: %v", zordonPID, err)
	}
	_ = cmd.Wait()

	// Upper bound: runShell grace + alpha shutdownGrace + slack.
	// Provision's grace defaults to cfg.shutdownGrace (2s); plus
	// shutdownAll cleanup window; 8s is comfortable and still small
	// enough that a regression to "alpha waits for sleep 30 to finish"
	// fails loudly.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(alphaPID) {
			t.Logf("alpha pid %d reaped after Ctrl-C — runShell escalation works", alphaPID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(alphaPID, syscall.SIGKILL)
	t.Fatalf("alpha pid %d still alive 8s after zordon SIGINT — runShell/shutdownAll did not converge", alphaPID)
}
