//go:build conformance_go

// `zordon status` is a read-only query and must survive being run at any
// point of a bringup. It sends OpState, which alpha answers from
// alphaState.snapshot() — and snapshot walks the serviceCtx map that
// handleConfigure pre-populates BEFORE toolchains, checkout and build run.
// For the whole build window a registered service therefore has no process
// yet. Reading a pid off the not-yet-assigned *exec.Cmd panicked the handler
// goroutine, and with no recover in the process that panic took alpha — and
// the `zordon start` blocked on its event stream — down with it.
//
// The build here sleeps before doing the real `go build`, which widens that
// pre-spawn window to something a second process can reliably land in.
package conformance_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

func TestStatusDuringBringup_startSurvives(t *testing.T) {
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
  src { path = "./src/svc1" }

  build {
    cmd = ["/bin/sh", "-c", "echo build-running; sleep 10; go build -o \"${fs::bin()}/svc1\" ."]
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

	t.Cleanup(func() { dumpAlphaLog(t, p.AlphaLogPath()) })

	// Spawn zordon directly: the harness's Start() blocks until exit, and
	// we need to talk to alpha while the bringup is still in flight.
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, zordonBinaryFromEnvOrPath(t),
		"start",
		"--timeout", "150s",
		"--alpha-log", p.AlphaLogPath(),
	)
	cmd.Dir = p.Dir()
	cmd.Env = append(os.Environ(),
		"ZORDON_HOME="+p.Home(),
		"ZORDON_TEST_HARNESS=1",
	)
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
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if s, e := os.ReadFile(stdout.Name()); e == nil && len(s) > 0 {
			t.Logf("zordon stdout:\n%s", s)
		}
		if s, e := os.ReadFile(stderr.Name()); e == nil && len(s) > 0 {
			t.Logf("zordon stderr:\n%s", s)
		}
	})

	// "build-running" is the build cmd's first echo: prepareBuild has
	// spawned it, so the service is registered but its runtime process
	// does not exist yet — exactly the window that used to crash alpha.
	waitForLogContent(t, p.AlphaLogPath(), "build-running", 120*time.Second)
	alphaPID := lastAlphaPIDFromLog(t, p.AlphaLogPath())
	if alphaPID <= 0 {
		t.Fatalf("no alpha pid in %s", p.AlphaLogPath())
	}

	res := p.Zordon("status").Run(t)
	out := res.Stdout + res.Stderr
	if res.ExitCode != 0 {
		t.Fatalf("zordon status exit=%d during bringup, want 0\n%s", res.ExitCode, out)
	}
	// Not "running pid=0 [unhealthy: ...]": alpha reports no readiness for
	// a service it has not spawned, and status must not invent a live probe
	// against a port nobody bound yet.
	if !strings.Contains(out, "svc1 — starting") {
		t.Errorf("status does not report svc1 as starting mid-build:\n%s", out)
	}

	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("zordon start failed after a concurrent status: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("zordon start did not finish: %v", ctx.Err())
	}

	if !pidAlive(alphaPID) {
		t.Errorf("alpha pid %d died during the bringup", alphaPID)
	}
	if log := p.AlphaLog().Contents(); strings.Contains(log, "panic:") {
		t.Errorf("alpha log contains a panic:\n%s", log)
	}
}
