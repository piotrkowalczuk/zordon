//go:build conformance_go

package conformance_test

import (
	"testing"
	"time"

	"github.com/piotrkowalczuk/zordon/internal/zordontest"
)

// The documented shape for "prepare state with the built artifact, then
// start the daemon": a provision gated on self.build.success, and the
// runtime gated on that provision (Rails' db:prepare needs the bundle the
// build installed; the server needs the database). runtime.after gates the
// cmd, not the build — when alpha waited on runtime.after before building,
// this was a silent deadlock: the build waited on the provision, the
// provision on the build, and alpha hung without a word until the
// timeout. The provision also asserts the artifact already exists, so a
// provision that ran early would fail rather than pass by luck.
func TestProvision_afterOwnBuild_gatesRuntime(t *testing.T) {
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
  src {
    path = "./src/svc1"
    exe  = "."
  }

  vars = { port = net::pickport() }

  runtime {
    after = [self.runtime.provision.prepare.success]

    provision "prepare" {
      after = [self.build.success]
      cmd   = "test -x ${fs::bin()}/svc1"
    }

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

	p.Start(t, zordontest.StartTimeout(3*time.Minute)).OK()
	mustDecodeEcho(t, p.Get(t, "service.go.svc1.vars.port").Int())
}
