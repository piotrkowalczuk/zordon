//go:build conformance_rust

package conformance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Rust variant of the exe-anchor conformance — see exe_anchor_go_test.go
// for the invariant this pins.

func TestExeAnchor_Rust(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/rust/echo", "src/cmd/echo")

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  rust {
    version = "%s"
  }
}

service "rust" "echo" {
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
`, rustVersion))

	mustStart(t, p)
	port := p.Get(t, "service.rust.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if !strings.Contains(echo.RuntimeVersion, rustVersion) {
		t.Errorf("runtime_version = %q; want it to contain pinned rust %s", echo.RuntimeVersion, rustVersion)
	}
	assertCwdEndsWithExe(t, echo.Cwd, "src", "cmd", "echo")
}
