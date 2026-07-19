//go:build conformance_go

package conformance_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Workspace conformance: per-toolchain end-to-end through
// `zordon workspace create <name> <svc>` + `zordon start` from inside
// the workspace dir.
//
// Same fixture, same JSON wire shape as the in-place per-toolchain
// suites — what's distinct is the path the service runs from:
//
//   - source materialised at <project>/workspaces/<wt>/src/<svc>
//     via `git worktree add` (so the user can edit it in isolation);
//   - alpha and its sockets live under that workspace's state dir;
//   - fs::bin() is per-workspace, so builds don't trample main's.
//
// Cwd in the echoed JSON must point at the per-workspace checkout —
// the most observable invariant a regression would break.
//
// The per-language variants are split across workspace_<lang>_test.go
// so each lands on its own toolchain leg; the shared helpers
// (mustWorkspaceStart, mustCwdInWorkspace, mustZordonOK) live in
// shared_test.go.

func TestWorkspace_Go(t *testing.T) {
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

	port := mustWorkspaceStart(t, p, "wt1", "go", "echo")
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "go1.26") {
		t.Errorf("runtime_version = %q; want go1.26.x (pinned toolchain)", echo.RuntimeVersion)
	}
	mustCwdInWorkspace(t, echo.Cwd, p, "wt1", "echo")
}

// TestWorkspace_Go_exeOffset is the workspace counterpart of the exe_anchor
// suite: a workspace service whose build target lives in a SUBDIR
// (exe != "."). The git worktree must check out at the checkout ROOT
// (<wtdir>/src/<svc>) while build+run anchor on <root>/<exe>. This
// regressed when the workspace-add target was taken from the (now
// exe-anchored) service Dir instead of the checkout root: `zordon start`
// then tried to add the workspace at <root>/<exe>, colliding with the
// root that `zordon workspace create` had already registered ("branch
// already checked out"). The plain TestWorkspace_* cases all use exe="."
// (root == exe dir), so none of them could catch it.
func TestWorkspace_Go_exeOffset(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/repo/cmd/echo")
	p.GitInit("src/repo")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "echo" {
  src {
    path = "./src/repo"
    exe  = "./cmd/echo"
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

	port := mustWorkspaceStart(t, p, "wt1", "go", "echo")
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "go1.26") {
		t.Errorf("runtime_version = %q; want go1.26.x (pinned toolchain)", echo.RuntimeVersion)
	}
	// cwd is the exe-anchored dir INSIDE the per-workspace checkout:
	// <wtdir>/src/echo/cmd/echo — proving the workspace checked out at the
	// root while build/run anchored on the exe subdir.
	wantSuffix := filepath.Join("workspaces", "wt1", "src", "echo", "cmd", "echo")
	if !strings.HasSuffix(echo.Cwd, wantSuffix) {
		t.Errorf("service cwd = %q; want it to end with %q (exe-anchored within the workspace)", echo.Cwd, wantSuffix)
	}
}

// TestWorkspace_ServiceAddRm pins `zordon workspace service {add,rm}`: a
// service's per-workspace checkout can be detached and re-materialized in an
// existing workspace (git-only, no bringup), and the command rejects the main
// workspace and unknown targets. No toolchain needed — nothing is built.
func TestWorkspace_ServiceAddRm(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/echo")
	p.GitInit("src/echo")
	p.WriteFile("Alphasfile", `
service "go" "echo" {
  src {
    path = "./src/echo"
    exe  = "."
  }
}
`)

	mustZordonOK(t, p, "workspace", "create", "wt1", "echo")
	co := filepath.Join(p.Dir(), "workspaces", "wt1", "src", "echo")
	if !fileExists(co) {
		t.Fatalf("checkout %s missing after create", co)
	}

	mustZordonOK(t, p, "workspace", "service", "rm", "--workspace=wt1", "--services=echo")
	if fileExists(co) {
		t.Fatalf("checkout %s still present after `service rm`", co)
	}

	mustZordonOK(t, p, "workspace", "service", "add", "--workspace=wt1", "--services=echo")
	if !fileExists(filepath.Join(co, ".git")) {
		t.Fatalf("checkout %s not re-materialized after `service add`", co)
	}

	// Each of these must be rejected (non-zero exit): main has no per-service
	// checkout; the workspace doesn't exist; the service isn't declared.
	for _, bad := range [][]string{
		{"workspace", "service", "add", "--workspace=main", "--services=echo"},
		{"workspace", "service", "add", "--workspace=nope", "--services=echo"},
		{"workspace", "service", "add", "--workspace=wt1", "--services=ghost"},
		{"workspace", "service", "rm", "--workspace=wt1", "--services=ghost"},
	} {
		if res := p.Zordon(bad...).WithTimeout(time.Minute).Run(t); res.ExitCode == 0 {
			t.Errorf("zordon %s: exit 0, want failure", strings.Join(bad, " "))
		}
	}
}
