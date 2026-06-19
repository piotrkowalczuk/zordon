// Clean conformance: a provision's `clean` snippet is the teardown that
// `zordon clean` runs against a STOPPED stack — and that plain `zordon stop`
// must NOT run. These pin both halves: clean undoes the side effect, and a
// bare stop leaves it in place.
package conformance_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

const cleanAlphasfile = `
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

    # cmd creates a marker at bringup; clean removes it. A bare stop
    # leaves the marker; only zordon clean (stack down) runs the clean
    # snippet. fs::state() bakes the absolute state dir at eval time, so
    # the snippet needs no runtime env.
    provision "marker" {
      cmd   = "mkdir -p ${fs::state()}/markers && touch ${fs::state()}/markers/clean-me"
      clean = "rm -f ${fs::state()}/markers/clean-me"
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
`

// TestClean_zordonClean pins the core contract: after a start, the marker
// exists; a plain stop leaves it (clean is NOT part of stop); a subsequent
// `zordon clean` against the stopped stack removes it.
func TestClean_zordonClean(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	p.WriteFile("Alphasfile", cleanAlphasfile)

	marker := filepath.Join(p.Dir(), "workspaces", "main", "markers", "clean-me")

	start := p.Zordon("start", "--timeout", "120s", "--alpha-log", p.AlphaLogPath()).
		WithTimeout(125 * time.Second).Run(t)
	if start.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstdout: %s\nstderr: %s", start.ExitCode, start.Stdout, start.Stderr)
	}
	if !fileExists(marker) {
		t.Fatalf("marker %s missing after start — provision cmd didn't run", marker)
	}

	// Grab the socket before stop so we can wait for the stack to be fully
	// down — `zordon clean` refuses a running stack, and stop is async.
	sock := parseSocketPath(t, p.AlphaLogPath())

	stop := p.Zordon("stop").WithTimeout(10 * time.Second).Run(t)
	if stop.ExitCode != 0 {
		t.Fatalf("zordon stop: exit %d\nstderr: %s", stop.ExitCode, stop.Stderr)
	}
	if !fileExists(marker) {
		t.Fatalf("marker %s removed by plain stop — clean must NOT run on stop", marker)
	}
	if err := waitGone(func() bool { _, err := os.Stat(sock); return os.IsNotExist(err) }, 10*time.Second); err != nil {
		t.Fatalf("alpha socket %s still present after stop: %v", sock, err)
	}

	clean := p.Zordon("clean", "--timeout", "120s").WithTimeout(125 * time.Second).Run(t)
	if clean.ExitCode != 0 {
		t.Fatalf("zordon clean: exit %d\nstdout: %s\nstderr: %s", clean.ExitCode, clean.Stdout, clean.Stderr)
	}
	if fileExists(marker) {
		t.Fatalf("marker %s still present after zordon clean — clean snippet didn't run", marker)
	}

	p.AssertNoLeftovers(t)
}

// TestClean_zordonCleanRunningStack pins the guard: clean refuses to run
// while the stack is up (it operates on a stopped stack), directing the user
// to stop first.
func TestClean_zordonCleanRunningStack(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	p.WriteFile("Alphasfile", cleanAlphasfile)

	start := p.Zordon("start", "--timeout", "120s", "--alpha-log", p.AlphaLogPath()).
		WithTimeout(125 * time.Second).Run(t)
	if start.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstderr: %s", start.ExitCode, start.Stderr)
	}

	clean := p.Zordon("clean").WithTimeout(15 * time.Second).Run(t)
	if clean.ExitCode == 0 {
		t.Fatalf("zordon clean succeeded against a running stack; want a refusal\nstdout: %s\nstderr: %s", clean.Stdout, clean.Stderr)
	}
}

// TestClean_stopClean pins the sugar: `zordon stop --clean` brings the stack
// down AND runs the clean snippets in one shot.
func TestClean_stopClean(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/svc1")
	p.WriteFile("Alphasfile", cleanAlphasfile)

	marker := filepath.Join(p.Dir(), "workspaces", "main", "markers", "clean-me")

	start := p.Zordon("start", "--timeout", "120s", "--alpha-log", p.AlphaLogPath()).
		WithTimeout(125 * time.Second).Run(t)
	if start.ExitCode != 0 {
		t.Fatalf("zordon start: exit %d\nstderr: %s", start.ExitCode, start.Stderr)
	}
	if !fileExists(marker) {
		t.Fatalf("marker %s missing after start", marker)
	}

	sc := p.Zordon("stop", "--clean", "--timeout", "120s").WithTimeout(130 * time.Second).Run(t)
	if sc.ExitCode != 0 {
		t.Fatalf("zordon stop --clean: exit %d\nstdout: %s\nstderr: %s", sc.ExitCode, sc.Stdout, sc.Stderr)
	}
	if fileExists(marker) {
		t.Fatalf("marker %s still present after stop --clean", marker)
	}

	p.AssertNoLeftovers(t)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
