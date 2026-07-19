//go:build conformance_node

package conformance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// Node variant of the workspace conformance — see workspace_go_test.go
// for the invariant this pins.

func TestWorkspace_Nodejs(t *testing.T) {
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

	port := mustWorkspaceStart(t, p, "wt1", "nodejs", "echo")
	echo := mustDecodeEcho(t, port)
	if !strings.HasPrefix(echo.RuntimeVersion, "v22.") {
		t.Errorf("runtime_version = %q; want v22.x (pinned toolchain)", echo.RuntimeVersion)
	}
	mustCwdInWorkspace(t, echo.Cwd, p, "wt1", "echo")
}
