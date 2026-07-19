//go:build conformance_node

package conformance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Node variant of the exe-anchor conformance — see exe_anchor_go_test.go
// for the invariant this pins.

func TestExeAnchor_Nodejs(t *testing.T) {
	p := zordontest.NewProject(t)
	p.CopyTree("golden/nodejs/echo", "src/cmd/echo")

	p.WriteFile("Alphasfile", fmt.Sprintf(`
sysenv = ["HOME", "USER", "PATH", "LANG", "TMPDIR"]
toolchain {
  nodejs {
    version = "%s"
  }
}

service "nodejs" "echo" {
  src {
    path = "./src"
    exe  = "./cmd/echo"
  }

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

	mustStart(t, p)
	port := p.Get(t, "service.nodejs.echo.vars.port").Int()
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "v22.") {
		t.Errorf("runtime_version = %q; want v22.x (pinned toolchain)", echo.RuntimeVersion)
	}
	assertCwdEndsWithExe(t, echo.Cwd, "src", "cmd", "echo")
}
