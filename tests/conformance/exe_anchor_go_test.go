//go:build conformance_go

package conformance_test

import (
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Exe-anchor conformance: pin the rule that `<src.path>/<exe>` is the
// unified working directory for both build and runtime, across every
// toolchain. The fixtures are arranged so `exe ≠ "."`:
//
//	<project>/Alphasfile
//	<project>/src/cmd/echo/{main.go, Cargo.toml, package.json}
//
// `src.path = "./src"` + `exe = "./cmd/echo"` — the module manifest
// (go.mod / Cargo.toml / package.json) lives at `<src>/cmd/echo/`,
// NOT at `<src>/`. The old behavior built from cwd = `<src>` with
// `./cmd/echo` as a path argument; for Go that means running
// `go build ./cmd/echo` from a dir with no go.mod, which fails.
// The unified behavior builds from cwd = `<src>/cmd/echo` where
// the manifest sits, so `go build .` / `cargo install --path .` /
// `npm install` resolve naturally.
//
// The runtime assertion (echoed cwd ends with the exe offset) is
// the cleanest negative test for the old behavior: there, runtime
// cwd was `<dest>` and the suffix wouldn't match.
//
// The per-language variants are split across exe_anchor_<lang>_test.go
// so each lands on its own toolchain leg; the shared assertion helper
// assertCwdEndsWithExe lives in shared_test.go.

func TestExeAnchor_Go(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/go/echo", "src/cmd/echo")

	p.WriteFile("Alphasfile", `
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  go {
    version = "1.26.2"
  }
}

service "go" "echo" {
  src {
    path = "./src"
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

	mustStart(t, p)
	port := p.Get(t, "service.go.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "go1.26") {
		t.Errorf("runtime_version = %q; want go1.26.x (pinned toolchain)", echo.RuntimeVersion)
	}
	assertCwdEndsWithExe(t, echo.Cwd, "src", "cmd", "echo")
}
