package conformance_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Worktree conformance: per-toolchain end-to-end through
// `zordon worktree create <name> <svc>` + `zordon start` from inside
// the worktree dir.
//
// Same fixture, same JSON wire shape as the in-place per-toolchain
// suites — what's distinct is the path the service runs from:
//
//   - source materialised at <project>/.zordon/worktrees/<wt>/src/<svc>
//     via `git worktree add` (so the user can edit it in isolation);
//   - alpha and its sockets live under that worktree's state dir;
//   - fs::bin() is per-worktree, so builds don't trample main's.
//
// Cwd in the echoed JSON must point at the per-worktree checkout —
// the most observable invariant a regression would break.

func TestWorktree_Go(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/echo")
	p.GitInit("src/echo")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "echo" {
  src {
    path = "./src/echo"
    exe  = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`)

	port := mustWorktreeStart(t, p, "wt1", "go", "echo")
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "go1.26") {
		t.Errorf("runtime_version = %q; want go1.26.x (pinned toolchain)", echo.RuntimeVersion)
	}
	mustCwdInWorktree(t, echo.Cwd, p, "wt1", "echo")
}

func TestWorktree_Rust(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/echo")
	p.WriteFile("src/echo/.gitignore", "target/\n")
	p.GitInit("src/echo")

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
  src {
    path = "./src/echo"
    exe  = "."
  }

  vars = { port = net::pickport() }

  runtime {
    cmd = ["${fs::bin()}/echo", "-addr", "127.0.0.1:${self.vars.port}"]
  }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, rustVersion))

	port := mustWorktreeStart(t, p, "wt1", "rust", "echo")
	echo := mustDecodeEcho(t, port)
	if !strings.Contains(echo.RuntimeVersion, rustVersion) {
		t.Errorf("runtime_version = %q; want it to contain pinned rust %s", echo.RuntimeVersion, rustVersion)
	}
	mustCwdInWorktree(t, echo.Cwd, p, "wt1", "echo")
}

func TestWorktree_Nodejs(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/echo")
	p.GitInit("src/echo")

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src { path = "./src/echo" }

  vars = { port = net::pickport() }
  env  = { PORT = "${self.vars.port}" }

  readiness {
    http {
      path = "/"
      port = self.vars.port
    }
    period            = "200ms"
    failure_threshold = 50
  }
}
`, nodeVersion))

	port := mustWorktreeStart(t, p, "wt1", "nodejs", "echo")
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "v22.") {
		t.Errorf("runtime_version = %q; want v22.x (pinned toolchain)", echo.RuntimeVersion)
	}
	mustCwdInWorktree(t, echo.Cwd, p, "wt1", "echo")
}

// mustWorktreeStart runs `zordon worktree create <wt> <svc>` and then
// `zordon start` from inside the worktree dir. The two halves are split
// so a failure in `worktree create` (git-init missing, bad checkout)
// surfaces with its own error before alpha is involved. Returns the
// port the service bound: each fixture uses net::pickport(), so the
// value is read back from the running worktree alpha (get must run from
// the worktree dir, where that alpha lives) rather than hardcoded.
func mustWorktreeStart(t *testing.T, p *zordontest.Project, wt, tc, svc string) int {
	t.Helper()
	res := p.Zordon("worktree", "create", wt, svc).WithTimeout(5 * time.Minute).Run(t)
	if res.ExitCode != 0 {
		t.Fatalf("zordon worktree create %s %s: exit %d\nstdout: %s\nstderr: %s",
			wt, svc, res.ExitCode, res.Stdout, res.Stderr)
	}
	wtRel := filepath.Join(".zordon", "worktrees", wt)
	p.Start(t, zordontest.StartIn(wtRel)).OK()
	return p.GetIn(t, wtRel, fmt.Sprintf("service.%s.%s.vars.port", tc, svc)).Int()
}

// mustCwdInWorktree pins the observable shape of a worktree run: the
// spawned service's cwd is its per-worktree checkout dir, not the
// project root and not the main worktree's checkout.
func mustCwdInWorktree(t *testing.T, cwd string, p *zordontest.Project, wt, svc string) {
	t.Helper()
	want := filepath.Join(p.Dir(), ".zordon", "worktrees", wt, "src", svc)
	// macOS sometimes returns /private/var/... for /var/... — match by
	// suffix so the assertion isn't flaky on the symlinked tmpdir.
	if !strings.HasSuffix(cwd, filepath.Join(".zordon", "worktrees", wt, "src", svc)) {
		t.Errorf("service cwd = %q; want a path ending with %q (the per-worktree checkout, not the anchor)",
			cwd, want)
	}
}
