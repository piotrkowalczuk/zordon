// End-to-end reproduction of the "alpha hangs because build can't be
// cancelled" bug. Pinned scenario:
//
//   1. service has an explicit build cmd that traps SIGTERM and sleeps
//      for ~30s — mirrors the real-world failure mode (a `set -euo
//      pipefail; xargs -P` block, a heavy `cargo install`, a misbehaving
//      build helper).
//   2. zordon spawns alpha, alpha logs "prepare svc1: build (...)",
//      i.e. the build subprocess is running.
//   3. test SIGINTs zordon. parentwatch in alpha fires, requestShutdown
//      closes shutdownCh, state.shutdownAll requests stop on every
//      service, sc.done is supposed to close.
//   4. assertion: alpha process is gone within shutdownGrace + small
//      slack (~5s total). Without the fix shipped in this PR, alpha
//      would block in bringupAndSupervise → prepareBuild → c.Wait()
//      until the trap-then-sleep finishes (~30s), and a follow-up
//      `zordon start` would happily spawn a second alpha on top.
//
// The trap-then-sleep is the load-bearing detail: a build that exits
// on SIGTERM would mask the bug because c.Wait would return naturally
// even without ctx-aware cancellation. We want the full reaper path
// (ctx.Done → SIGTERM → grace → SIGKILL → reap) actually exercised.
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

func TestStuckBuild_zordonSIGINTReapsAlphaWithinGrace(t *testing.T) {
	// SIGINT bypasses zordon's normal cleanup, so the harness's stop
	// step will race whatever alpha is doing. Same reason as
	// parentwatch_test.go and restart_test.go: skip the leftover
	// assertion, we sweep ourselves at the end.
	p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
	p.CopyTree("golden/go/echo", "src/svc1")

	const port = 27971
	// Build sleeps 30s while ignoring SIGTERM. trap '' TERM is a bash
	// idiom for "swallow this signal silently" — exactly the misbehaving
	// build the supervisor must still be able to reap.
	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src { path = "./src/svc1" }

  build {
    cmd = ["/bin/sh", "-c", "trap '' TERM; echo build-running; sleep 30"]
  }

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
		// Belt-and-braces: if we somehow leak zordon, don't pollute
		// the host. Process.Kill is no-op on an already-reaped pid.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the build to actually start running — that's the
	// window where the bug manifests. "build-running" comes from the
	// build cmd's first echo; reaching it proves prepareBuild has
	// entered runner → c.Start has succeeded → the trap is installed
	// and the sleep is in flight.
	waitForLogContent(t, p.AlphaLogPath(), "build-running", 60*time.Second)
	alphaPID := lastAlphaPIDFromLog(t, p.AlphaLogPath())
	if alphaPID <= 0 {
		t.Fatalf("could not read alpha pid from log")
	}
	t.Logf("alpha pid=%d, build is sleeping with SIGTERM trapped — SIGINT zordon now", alphaPID)

	if err := syscall.Kill(zordonPID, syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT zordon pid %d: %v", zordonPID, err)
	}
	_ = cmd.Wait()

	// Upper bound: prepareRunner's grace is 2s before SIGKILL. Add
	// alpha-side shutdownGrace (default 2s) and slack for scheduler
	// hiccups; 8s is comfortable and still small enough that the
	// pre-fix behavior (alpha blocked ~30s on c.Wait) fails loudly.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(alphaPID) {
			t.Logf("alpha pid %d reaped after Ctrl-C — fix works", alphaPID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Test failed: clean up the leaked alpha so the host stays sane,
	// then fail loudly with enough context to pinpoint the regression.
	_ = syscall.Kill(alphaPID, syscall.SIGKILL)
	t.Fatalf("alpha pid %d still alive 8s after zordon SIGINT — prepareBuild's c.Wait blocked the shutdownAll path", alphaPID)
}

// waitForLogContent + lastAlphaPIDFromLog live in log_helpers_test.go
// so stuck_provision_test (and future siblings) can share them without
// crossing test-file include boundaries.
