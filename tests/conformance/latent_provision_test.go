//go:build conformance_go

// Latent-provision shutdown conformance: a service with a latent
// provision (after = never) must not deadlock alpha's shutdown.
//
// Bug: in handleConfigure the latent provision's pc is registered in
// state.provisions but no `go runProvision(pc, ...)` is ever spawned —
// latents are CmdRef templates, not real entities to run. The pc.done
// channel is therefore never closed by anything. shutdownAll's
//
//	for _, pc := range provisions {
//	    <-pc.done
//	}
//
// hangs on that one latent forever. Alpha then survives every
// OpShutdown and every Ctrl-C of zordon, and pgrep stacks alphas
// across restarts (each new `zordon start` spawns a new alpha because
// the old one's socket is gone but the process is still alive).
//
// Reproduction shape:
//  1. Alphasfile with a normal service plus a latent provision.
//  2. `zordon start` — bringup completes cleanly (latent doesn't run,
//     service comes up).
//  3. `zordon stop` — sends OpShutdown; without the fix alpha hangs
//     indefinitely in shutdownAll. With the fix alpha exits promptly.
package conformance_test

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestLatentProvision_shutdownDoesNotDeadlock(t *testing.T) {
	p := zordontest.NewProject(t, zordontest.WithExpectedLeftovers())
	p.CopyTree("golden/go/echo", "src/svc1")

	// Single service + one latent provision. The latent never runs
	// (after = never) — it would only be invoked by a peer via
	// `cmd_ref = create_topic` (none here; we don't need to exercise
	// the peer path to trigger the deadlock, just the registration).
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

    provision "create_topic" {
      after = never
      cmd   = "echo template-body-should-never-run"
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
`)

	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	// Phase 1: clean bringup. Alpha must start, configure, return zero
	// — the latent isn't expected to run, only register.
	res := p.Zordon("start",
		"--timeout", "60s",
		"--alpha-log", p.AlphaLogPath(),
	).WithTimeout(90 * time.Second).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}

	alphaPID := lastAlphaPIDFromLog(t, p.AlphaLogPath())
	if alphaPID <= 0 || !pidAlive(alphaPID) {
		t.Fatalf("alpha pid %d not alive after bringup; alpha.log mid-flight?", alphaPID)
	}
	t.Logf("bringup OK, alpha pid=%d alive, latent provision registered — now zordon stop", alphaPID)

	// Phase 2: zordon stop drives OpShutdown. Alpha must exit within
	// a small window. Pre-fix: hangs on <-pc.done in shutdownAll for
	// the latent provision whose done channel nobody ever closes —
	// zordon stop itself returns quickly (OpShutdown is fire-and-
	// forget), but alpha lingers indefinitely. We bound the wait at
	// 10s: shutdownGrace is 2s + slack.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	stopRes := p.Zordon("stop").WithTimeout(10 * time.Second).Run(t)
	if stopRes.ExitCode != 0 {
		t.Logf("zordon stop exit %d (continuing; the assertion is on alpha):\nstdout: %s\nstderr: %s",
			stopRes.ExitCode, stopRes.Stdout, stopRes.Stderr)
	}
	_ = stopCtx

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(alphaPID) {
			t.Logf("alpha pid %d exited after zordon stop — latent didn't deadlock shutdownAll", alphaPID)
			return
		}
	}
	_ = syscall.Kill(alphaPID, syscall.SIGKILL)
	t.Fatalf("alpha pid %d still alive 10s after zordon stop — shutdownAll deadlocked on latent provision's never-closed pc.done", alphaPID)
}

// Compile-time hint that exec is intentionally imported for future
// expansion (driving stop via the binary directly if Zordon helper is
// ever removed). Kept silent for now.
var _ = exec.Command
