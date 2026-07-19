//go:build conformance_go

// Grandchild-escape conformance for the BUILD path.
//
// newPrepareRunner serializes its two waits — wg.Wait() (stream
// scanner drain) THEN c.Wait() — inside a single goroutine that
// feeds waitErr. That ordering is safe when the build subprocess
// is the sole owner of its stdout/stderr write fds: SIGKILL on pgid
// kills bash, bash's pipe fds close, scanners EOF, wg.Wait returns,
// c.Wait reaps, waitErr fires, runner unblocks.
//
// It is NOT safe when a grandchild escapes the pgid via setsid /
// nohup / Process.daemon — the grandchild's inherited write fd keeps
// the pipe alive, scanners never EOF, wg.Wait never returns, c.Wait
// is never called, and the `<-waitErr` at the bottom of the cancel
// path deadlocks. Real-world triggers: Spring (Rails preloader)
// forked by bundle install, mise reshim helpers, npm's
// update-notifier daemon.
//
// We exercise the BUILD path specifically (not provision via
// runShell, which DOES start c.Wait independently and is therefore
// not vulnerable to this exact deadlock). The build cmd:
//  1. installs SIGTERM trap so SIGTERM doesn't kill it before grace
//  2. forks a python that setsid's into a new session and sleeps
//     120s while holding the inherited stdout/stderr write fds
//  3. sleeps 30s itself so the cancel race window is open
package conformance_test

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestGrandchildEscape_buildReapsAlphaWithinGrace(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; needed to spawn a grandchild that setsid's out of the pgid")
	}

	p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
	p.CopyTree("golden/go/echo", "src/svc1")

	// build cmd:
	//   trap '' TERM        — swallow SIGTERM so we always test the
	//                          SIGKILL+fd-escape path, not the fast
	//                          "process exited on SIGTERM" path
	//   python3 ... &       — fork grandchild that setsid's; it
	//                          inherits the bash pipe fds, escapes the
	//                          pgid, and sleeps 120s
	//   echo build-running  — proves we got past the python spawn so
	//                          the test waiter can SIGINT zordon
	//   sleep 30            — keeps bash alive so the cancel race
	//                          actually races a live subprocess
	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "svc1" {
  src { path = "./src/svc1" }

  build {
    cmd = ["/bin/sh", "-c", "trap '' TERM; python3 -c 'import os,time; os.setsid(); time.sleep(120)' & echo build-running; sleep 30"]
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

	waitForLogContent(t, p.AlphaLogPath(), "build-running", 60*time.Second)
	alphaPID := lastAlphaPIDFromLog(t, p.AlphaLogPath())
	if alphaPID <= 0 {
		t.Fatalf("could not read alpha pid from log")
	}
	t.Logf("alpha pid=%d, grandchild has setsid'd and is holding pipe fds — SIGINT zordon now", alphaPID)

	if err := syscall.Kill(zordonPID, syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT zordon pid %d: %v", zordonPID, err)
	}
	_ = cmd.Wait()

	// Upper bound: prepareRunner cancel path is SIGTERM + 2s grace +
	// SIGKILL + unbounded wait (the bug). With a correct fix that
	// closes pipes or starts c.Wait independently of wg.Wait, total is
	// under ~5s. 15s is loose enough to absorb scheduler hiccups; if
	// the wait is truly unbounded (the deadlock) we hit it cleanly.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(alphaPID) {
			t.Logf("alpha pid %d reaped after Ctrl-C — build path handled grandchild fd-escape", alphaPID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(alphaPID, syscall.SIGKILL)
	t.Fatalf("alpha pid %d still alive 15s after zordon SIGINT — newPrepareRunner's wg.Wait→c.Wait ordering deadlocked on escaped grandchild fd", alphaPID)
}
